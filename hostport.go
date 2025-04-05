package kubehostport

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	core "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/dnsutil"
	"github.com/coredns/coredns/plugin/pkg/fall"
	"github.com/coredns/coredns/request"
)

// KubeHostport is a plugin that creates records for a Kubernetes cluster's nodes with specific pods.
type KubeHostport struct {
	Next  plugin.Handler
	Zones []string

	Fall fall.F
	ttl  uint32

	// Kubernetes API interface
	client     kubernetes.Interface
	controller cache.Controller
	indexer    cache.Indexer

	// selectors to filter pods
	namespace      string
	labelKey       string
	labelVal       string
	strictHostPort bool

	// concurrency control to stop controller
	stopLock sync.Mutex
	shutdown bool
	stopCh   chan struct{}
}

// New returns a initialized KubeHostport.
func New(zones []string) *KubeHostport {
	k := new(KubeHostport)
	k.Zones = zones
	k.ttl = defaultTTL
	k.namespace = defaultNamespace
	k.labelKey = defaultLabelKey
	k.labelVal = defaultLabelVal
	k.stopCh = make(chan struct{})
	return k
}

const (
	// defaultTTL to apply to all answers.
	defaultTTL = 5

	// defaultNamespace to watch for pods - empty means all namespaces.
	defaultNamespace = ""

	// defaultLabelKey to watch for pods.
	defaultLabelKey = "shturval.link/serviceName"

	// defaultLabelVal to watch for pods.
	defaultLabelVal = ""
)

// Name implements the Handler interface.
func (k *KubeHostport) Name() string { return pluginName }

// ServeDNS implements the plugin.Handler interface.
func (k *KubeHostport) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}

	qname := state.Name()
	zone := plugin.Zones(k.Zones).Matches(qname)
	if zone == "" || !supportedQtype(state.QType()) {
		return plugin.NextOrFailure(k.Name(), k.Next, ctx, w, r)
	}
	zone = state.QName()[len(qname)-len(zone):] // maintain case of original query
	state.Zone = zone

	if len(zone) == len(qname) {
		writeResponse(w, r, nil, nil, []dns.RR{k.soa()}, dns.RcodeSuccess)
		return dns.RcodeSuccess, nil
	}

	// handle reverse lookups
	if state.QType() == dns.TypePTR {
		if addr := dnsutil.ExtractAddressFromReverse(qname); addr != "" {
			objs, err := k.indexer.ByIndex("reverse", addr)
			if err != nil {
				return dns.RcodeServerFailure, err
			}
			if len(objs) == 0 {
				if k.Fall.Through(state.Name()) {
					return plugin.NextOrFailure(k.Name(), k.Next, ctx, w, r)
				}
				writeResponse(w, r, nil, nil, []dns.RR{k.soa()}, dns.RcodeNameError)
				return dns.RcodeNameError, nil
			}
			var records []dns.RR
			for _, obj := range objs {
				pod, ok := obj.(*core.Pod)
				if !ok {
					return dns.RcodeServerFailure, fmt.Errorf("unexpected %q from *Pod index", reflect.TypeOf(obj))
				}

				labelKey := k.getPodLabelValueWithNamespace(pod)
				if labelKey == "" {
					continue
				}

				records = append(records, &dns.PTR{
					Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: k.ttl},
					Ptr: dnsutil.Join(labelKey, k.Zones[0]),
				})
			}
			writeResponse(w, r, records, nil, nil, dns.RcodeSuccess)
			return dns.RcodeSuccess, nil
		}
	}

	labelValue := state.Name()[0 : len(qname)-len(zone)-1]
	if zone == "." {
		labelValue = state.Name()[0 : len(qname)-len(zone)]
	}

	// get the pod by key name from the indexer
	objs, err := k.indexer.ByIndex("labelValue", labelValue)
	if err != nil {
		return dns.RcodeServerFailure, err
	}

	if len(objs) == 0 {
		if k.Fall.Through(state.Name()) {
			return plugin.NextOrFailure(k.Name(), k.Next, ctx, w, r)
		}
		writeResponse(w, r, nil, nil, []dns.RR{k.soa()}, dns.RcodeNameError)
		return dns.RcodeNameError, nil
	}

	// build response records
	var records []dns.RR
	switch state.QType() {
	case dns.TypeA:
		for _, obj := range objs {
			pod, ok := obj.(*core.Pod)
			if !ok {
				return dns.RcodeServerFailure, fmt.Errorf("unexpected %q from *Pod index", reflect.TypeOf(obj))
			}

			if pod.Status.Phase != core.PodRunning {
				continue
			}

			if strings.Contains(pod.Status.HostIP, ":") {
				continue
			}
			if netIP := net.ParseIP(pod.Status.HostIP); netIP != nil {
				records = append(records, &dns.A{A: netIP,
					Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: k.ttl}})
			}
		}
	case dns.TypeAAAA:
		for _, obj := range objs {
			pod, ok := obj.(*core.Pod)
			if !ok {
				return dns.RcodeServerFailure, fmt.Errorf("unexpected %q from *Pod index", reflect.TypeOf(obj))
			}

			if pod.Status.Phase != core.PodRunning {
				continue
			}

			if !strings.Contains(pod.Status.HostIP, ":") {
				continue
			}

			if netIP := net.ParseIP(pod.Status.HostIP); netIP != nil {
				records = append(records, &dns.AAAA{AAAA: netIP,
					Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: k.ttl}})
			}
		}
	}

	writeResponse(w, r, records, nil, nil, dns.RcodeSuccess)
	return dns.RcodeSuccess, nil
}

func writeResponse(w dns.ResponseWriter, r *dns.Msg, answer, extra, ns []dns.RR, rcode int) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Rcode = rcode
	m.Authoritative = true
	m.Answer = answer
	m.Extra = extra
	m.Ns = ns
	w.WriteMsg(m)
}

func (k *KubeHostport) soa() *dns.SOA {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: k.Zones[0], Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: k.ttl},
		Ns:      dnsutil.Join("ns.dns", k.Zones[0]),
		Mbox:    dnsutil.Join("hostmaster.dns", k.Zones[0]),
		Serial:  uint32(time.Now().Unix()),
		Refresh: 7200,
		Retry:   1800,
		Expire:  86400,
		Minttl:  k.ttl,
	}
}

func supportedQtype(qtype uint16) bool {
	switch qtype {
	case dns.TypeA, dns.TypeAAAA, dns.TypePTR:
		return true
	default:
		return false
	}
}

// Ready implements the ready.Readiness interface.
func (k *KubeHostport) Ready() bool { return k.controller.HasSynced() }
