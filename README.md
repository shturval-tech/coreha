# coreha
## Name
coreha - serve A records of node IP-s for specific pods

## Description
CoreDNS plugin to discover IPv4 addresses of Kubernetes nodes, where deployed specific pods with hostPort.
Plugin will select pods with specified label key and answer on A requests IP-addresses of their nodes.

## Syntax
In-cluster deploy
```
kubeapi
coreha [ZONES...] {
  ttl TTL
  labelKey kubernetes.pod/label
  strictHostPort BOOL
}
```


For local deploy add path to kubeconfig
```
kubeapi {
    kubeconfig ~/.kube/config
}
```

## Example
```
.:1053 {
  kubeapi {
    kubeconfig ~/.kube/config
  }
  coreha coreha.my-cluster.shturval {
    ttl 15
    labelKey shturval.link/serviceName
    strictHostPort true
  }
}
```

## How to build
```
# assume go and git already setup
# clone coredns repo
git clone https://github.com/coredns/coredns.git && cd coredns/

# add plugin to plugin.cfg
echo "coreha:github.com/shturval-tech/coreha" >> ./plugin.cfg

# build coredns
make

# check plugins after build
./coredns -plugins
```