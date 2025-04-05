package kubehostport

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/kubeapi"

	core "k8s.io/api/core/v1"
)

const pluginName = "coreha"

var log = clog.NewWithPlugin(pluginName)

func init() { plugin.Register(pluginName, setup) }

func setup(c *caddy.Controller) error {
	k, err := parse(c)
	if err != nil {
		return plugin.Error(pluginName, err)
	}

	k.setWatch(context.Background())
	c.OnStartup(startWatch(k, dnsserver.GetConfig(c)))
	c.OnShutdown(stopWatch(k))

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		k.Next = next
		return k
	})

	return nil
}

func parse(c *caddy.Controller) (*KubeHostport, error) {
	var (
		kns *KubeHostport
		err error
	)

	i := 0
	for c.Next() {
		if i > 0 {
			return nil, plugin.ErrOnce
		}
		i++

		kns, err = parseStanza(c)
		if err != nil {
			return kns, err
		}
	}
	return kns, nil
}

// parseStanza parses a kubehostport stanza
func parseStanza(c *caddy.Controller) (*KubeHostport, error) {
	kns := New(plugin.OriginsFromArgsOrServerBlock(c.RemainingArgs(), c.ServerBlockKeys))

	for c.NextBlock() {
		switch c.Val() {
		case "namespace":
			args := c.RemainingArgs()
			if len(args) == 0 {
				return nil, c.ArgErr()
			}

			kns.namespace = args[0]
		case "labelKey":
			args := c.RemainingArgs()
			if len(args) == 0 {
				return nil, c.ArgErr()
			}

			kns.labelKey = args[0]
		case "labelVal":
			args := c.RemainingArgs()
			if len(args) == 0 {
				return nil, c.ArgErr()
			}

			kns.labelVal = args[0]
		case "strictHostPort":
			args := c.RemainingArgs()
			if len(args) == 0 {
				return nil, c.ArgErr()
			}
			strictHostPort, err := strconv.ParseBool(args[0])
			if err != nil {
				return nil, err
			}
			kns.strictHostPort = strictHostPort

		case "ttl":
			args := c.RemainingArgs()
			if len(args) == 0 {
				return nil, c.ArgErr()
			}
			t, err := strconv.Atoi(args[0])
			if err != nil {
				return nil, err
			}
			if t < 0 || t > 3600 {
				return nil, c.Errf("ttl must be in range [0, 3600]: %d", t)
			}
			kns.ttl = uint32(t)
		default:
			return nil, c.Errf("unknown property '%s'", c.Val())
		}
	}

	return kns, nil
}

func (k *KubeHostport) setWatch(ctx context.Context) {
	// define Pod controller and reverse lookup indexer
	k.indexer, k.controller = cache.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc: func(o v1.ListOptions) (runtime.Object, error) {
				o.LabelSelector = k.labelKey
				return k.client.CoreV1().Pods(k.namespace).List(ctx, o)
			},
			WatchFunc: func(o v1.ListOptions) (watch.Interface, error) {
				o.LabelSelector = k.labelKey
				return k.client.CoreV1().Pods(k.namespace).Watch(ctx, o)
			},
		},
		&core.Pod{},
		0,
		cache.ResourceEventHandlerFuncs{},
		cache.Indexers{
			"reverse": func(obj interface{}) ([]string, error) {
				pod, ok := obj.(*core.Pod)
				if !ok {
					return nil, errors.New("unexpected obj type")
				}

				if !k.checkPodRequirements(pod) {
					return nil, nil
				}

				return []string{pod.Status.HostIP}, nil
			},
			"labelValue": func(obj interface{}) ([]string, error) {
				pod, ok := obj.(*core.Pod)
				if !ok {
					return nil, errors.New("unexpected obj type")
				}

				if !k.checkPodRequirements(pod) {
					return nil, nil
				}

				labelValue := k.getPodLabelValueWithNamespace(pod)
				if labelValue == "" {
					return nil, nil
				}
				return []string{labelValue}, nil
			},
		},
	)
}

func startWatch(k *KubeHostport, config *dnsserver.Config) func() error {
	return func() error {
		// retrieve client from kubeapi plugin
		var err error
		k.client, err = kubeapi.Client(config)
		if err != nil {
			return err
		}

		// start the informer
		go k.controller.Run(k.stopCh)
		return nil
	}
}

func stopWatch(k *KubeHostport) func() error {
	return func() error {
		k.stopLock.Lock()
		defer k.stopLock.Unlock()
		if !k.shutdown {
			close(k.stopCh)
			k.shutdown = true
			return nil
		}
		return fmt.Errorf("shutdown already in progress")
	}
}

// checkPodRequirements checks if a pod meets the requirements for being resolved by the KubeHostport plugin.
// It verifies that the pod is running, has a hostIP, has the required label (with optional value), and if strictHostPort is set, it also checks if the pod has a hostPort.
// Parameters:
// - pod: The pod to be checked.
// Returns:
// - bool: true if the pod meets the requirements, false otherwise.
func (k *KubeHostport) checkPodRequirements(pod *core.Pod) bool {
	// sanity check
	if pod == nil {
		return false
	}

	// Exclude pod with Terminating state or without hostIp
	if pod.DeletionTimestamp != nil || pod.Status.HostIP == "" {
		return false
	}

	// Get only Ready pods
	for _, c := range pod.Status.Conditions {
		if c.Type == core.PodReady && c.Status != core.ConditionTrue {
			return false
		}
	}
	// check if the pod is running and has a hostIP
	// if pod.Status.Phase != core.PodRunning || pod.Status.HostIP == "" {
	// 	return false
	// }

	// check if the pod has the required label
	val, ok := pod.Labels[k.labelKey]
	if !ok {
		return false
	}

	// check label value if required
	if k.labelVal != "" && val != k.labelVal {
		return false
	}

	// if strictHostPort is not set, we can return early
	if !k.strictHostPort {
		return true
	}

	// check if the pod has a hostPort
	hasHostPort := false
	for _, c := range pod.Spec.Containers {
		for _, port := range c.Ports {
			if port.HostPort != 0 {
				hasHostPort = true
			}
		}
	}

	return hasHostPort
}

// getPodLabelValueWithNamespace returns the value of the specified label key for the given pod,
// along with the namespace of the pod. If the label key is not found or the label value does not match
// the specified value, an empty string is returned.
func (k *KubeHostport) getPodLabelValueWithNamespace(pod *core.Pod) string {
	val, ok := pod.Labels[k.labelKey]
	if !ok {
		return ""
	}

	if k.labelVal == "" || val == k.labelVal {
		return val + "." + pod.Namespace
	}

	return ""
}
