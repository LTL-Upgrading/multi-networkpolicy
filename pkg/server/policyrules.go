package server

import (
	"bytes"
	"fmt"
	//"os"
	"strings"

	mvlanv1 "github.com/k8snetworkplumbingwg/macvlan-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
)

const PolicyNetworkAnnotation = "k8s.v1.cni.cncf.io/policy-for"

/*
// GetChainLines parses a table's iptables-save data to find chains in the table.
// It returns a map of iptables.Chain to []byte where the []byte is the chain line
// from save (with counters etc.).
// Note that to avoid allocations memory is SHARED with save.
func GetChainLines(table Table, save []byte) map[Chain][]byte {
*/
type iptableBuffer struct {
	currentFilter map[utiliptables.Chain][]byte
	currentChain  map[utiliptables.Chain]bool
	activeChain   map[utiliptables.Chain]bool
	policyIndex   *bytes.Buffer
	ingressPorts  *bytes.Buffer
	ingressFrom   *bytes.Buffer
	egressPorts   *bytes.Buffer
	egressTo      *bytes.Buffer
	filterChains  *bytes.Buffer
	filterRules   *bytes.Buffer
}

func newIptableBuffer() *iptableBuffer {
	buf := &iptableBuffer{
		currentFilter: make(map[utiliptables.Chain][]byte),
		policyIndex:   bytes.NewBuffer(nil),
		ingressPorts:  bytes.NewBuffer(nil),
		ingressFrom:   bytes.NewBuffer(nil),
		egressPorts:   bytes.NewBuffer(nil),
		egressTo:      bytes.NewBuffer(nil),
		filterChains:  bytes.NewBuffer(nil),
		filterRules:   bytes.NewBuffer(nil),
		currentChain:  map[utiliptables.Chain]bool{},
		activeChain:   map[utiliptables.Chain]bool{},
	}
	return buf
}

func (ipt *iptableBuffer) Init(iptables utiliptables.Interface) {
	tmpbuf := bytes.NewBuffer(nil)
	tmpbuf.Reset()
	err := iptables.SaveInto(utiliptables.TableFilter, tmpbuf)
	if err != nil {
		klog.Error("failed to get iptable filter")
		return
	}
	ipt.currentFilter = utiliptables.GetChainLines(utiliptables.TableFilter, tmpbuf.Bytes())
	for k := range ipt.currentFilter {
		if strings.HasPrefix(string(k), "MACVLAN-") {
			ipt.currentChain[k] = true
		}
	}

	ipt.filterRules.Reset()
	ipt.filterChains.Reset()
	writeLine(ipt.filterChains, "*filter")

	// Make sure we keep stats for the top-level chains, if they existed
	// (which most should have because we created them above).
	for _, chainName := range []utiliptables.Chain{macvlanIngressChain, macvlanEgressChain} {
		ipt.activeChain[chainName] = true
		if chain, ok := ipt.currentFilter[chainName]; ok {
			writeBytesLine(ipt.filterChains, chain)
		} else {
			writeLine(ipt.filterChains, utiliptables.MakeChainLine(chainName))
		}
	}
}

// Reset clears iptableBuffer
func (ipt *iptableBuffer) Reset() {
	ipt.policyIndex.Reset()
	ipt.ingressPorts.Reset()
	ipt.ingressFrom.Reset()
	ipt.egressPorts.Reset()
	ipt.egressTo.Reset()
}

func (ipt *iptableBuffer) FinalizeRules() {
	for k := range ipt.activeChain {
		delete(ipt.currentChain, k)
	}
	for chainName := range ipt.currentChain {
		if chain, ok := ipt.currentFilter[chainName]; ok {
			writeBytesLine(ipt.filterChains, chain)
		}
		writeLine(ipt.policyIndex, "-X", string(chainName))
	}
	ipt.filterRules.Write(ipt.filterChains.Bytes())
	ipt.filterRules.Write(ipt.policyIndex.Bytes())
	ipt.filterRules.Write(ipt.ingressPorts.Bytes())
	ipt.filterRules.Write(ipt.ingressFrom.Bytes())
	ipt.filterRules.Write(ipt.egressPorts.Bytes())
	ipt.filterRules.Write(ipt.egressTo.Bytes())
	writeLine(ipt.filterRules, "COMMIT")
}

func (ipt *iptableBuffer) SyncRules(iptables utiliptables.Interface) error {
	/*
		fmt.Fprintf(os.Stderr, "========= filterRules\n")
		fmt.Fprintf(os.Stderr, "%s", ipt.filterRules.String())
		fmt.Fprintf(os.Stderr, "=========\n")
	*/
	return iptables.RestoreAll(ipt.filterRules.Bytes(), utiliptables.NoFlushTables, utiliptables.RestoreCounters)
}

func (ipt *iptableBuffer) IsUsed() bool {
	return (len(ipt.activeChain) != 0)
}

func (buf *iptableBuffer) renderIngress(s *Server, pod *v1.Pod, ingresses []mvlanv1.MacvlanNetworkPolicyIngressRule, policyNetworks []string) {
	for n, ingress := range ingresses {
		writeLine(buf.policyIndex, "-A", macvlanIngressChain,
			"-j", "MARK", "--set-xmark 0x0/0x30000")
		buf.renderIngressPorts(s, pod, n, ingress.Ports, policyNetworks)
		buf.renderIngressFrom(s, pod, n, ingress.From, policyNetworks)
		writeLine(buf.policyIndex, "-A", macvlanIngressChain,
			"-m", "mark", "--mark", "0x30000/0x30000", "-j", "RETURN")
	}
	writeLine(buf.policyIndex, "-A", macvlanIngressChain, "-j", "DROP")
}

func (buf *iptableBuffer) renderIngressPorts(s *Server, pod *v1.Pod, index int, ports []mvlanv1.MacvlanNetworkPolicyPort, policyNetworks []string) {
	chainName := utiliptables.Chain(fmt.Sprintf("MACVLAN-INGRESS-%d-PORTS", index))

	buf.activeChain[utiliptables.Chain(chainName)] = true
	// Create chain if not exists
	if chain, ok := buf.currentFilter[chainName]; ok {
		writeBytesLine(buf.filterChains, chain)
	} else {
		writeLine(buf.filterChains, utiliptables.MakeChainLine(chainName))
	}

	// Add jump from MACVLAN-INGRESS
	writeLine(buf.policyIndex, "-A", macvlanIngressChain, "-j", string(chainName))

	// Add skip rule if no ports
	if len(ports) == 0 {
		writeLine(buf.ingressPorts, "-A", string(chainName),
			"-m", "comment", "--comment", "\"no ingress ports, skipped\"",
			"-j", "MARK", "--set-xmark", "0x10000/0x10000")
		return
	}

	for _, port := range ports {
		proto := strings.ToLower(string(*port.Protocol))
		podinfo, err := s.podMap.GetPodInfo(pod)
		if err != nil {
			klog.Errorf("cannot get podinfo: %v", err)
			continue
		}
		for _, macvlanIF := range podinfo.MacvlanInterfaces {
			if !macvlanIF.CheckPolicyNetwork(policyNetworks) {
				continue
			}
			writeLine(buf.ingressPorts, "-A", string(chainName),
				"-m", "comment", "--comment", "\"comment\"", "-i", macvlanIF.InterfaceName,
				"-m", proto, "-p", proto, "--dport", port.Port.String(),
				"-j", "MARK", "--set-xmark", "0x10000/0x10000")
		}
	}
}

func (buf *iptableBuffer) renderIngressFrom(s *Server, pod *v1.Pod, index int, from []mvlanv1.MacvlanNetworkPolicyPeer, policyNetworks []string) {
	chainName := utiliptables.Chain(fmt.Sprintf("MACVLAN-INGRESS-%d-FROM", index))
	podinfo, err := s.podMap.GetPodInfo(pod)
	if err != nil {
		klog.Errorf("cannot get podinfo: %v", err)
		return
	}

	buf.activeChain[utiliptables.Chain(chainName)] = true
	// Create chain if not exists
	if chain, ok := buf.currentFilter[chainName]; ok {
		writeBytesLine(buf.filterChains, chain)
	} else {
		writeLine(buf.filterChains, utiliptables.MakeChainLine(chainName))
	}
	// Add jump from MACVLAN-INGRESS
	writeLine(buf.policyIndex, "-A", macvlanIngressChain, "-j", string(chainName))

	// Add skip rule if no froms
	if len(from) == 0 {
		writeLine(buf.ingressFrom, "-A", string(chainName),
			"-m", "comment", "--comment", "\"no ingress from, skipped\"",
			"-j", "MARK", "--set-xmark", "0x20000/0x20000")
		return
	}

	for _, peer := range from {
		if peer.PodSelector != nil {
			podSelectorMap, err := metav1.LabelSelectorAsMap(peer.PodSelector)
			if err != nil {
				klog.Errorf("pod selector: %v", err)
				continue
			}
			podLabelSelector := labels.Set(podSelectorMap).AsSelectorPreValidated()
			pods, err := s.podLister.Pods(metav1.NamespaceAll).List(podLabelSelector)
			if err != nil {
				klog.Errorf("pod list failed:%v", err)
				continue
			}

			var nsSelector labels.Selector
			if peer.NamespaceSelector != nil {
				nsSelectorMap, err := metav1.LabelSelectorAsMap(peer.NamespaceSelector)
				if err != nil {
					klog.Errorf("namespace selector: %v", err)
					continue
				}
				nsSelector = labels.Set(nsSelectorMap).AsSelectorPreValidated()
			}
			s.namespaceMap.Update(s.nsChanges)

			for _, sPod := range pods {
				nsLabels, err := s.namespaceMap.GetNamespaceInfo(sPod.Namespace)
				if err != nil {
					klog.Errorf("cannot get namespace info: %v %v", sPod.ObjectMeta.Name, err)
					continue
				}
				if nsSelector != nil && !nsSelector.Matches(labels.Set(nsLabels.Labels)) {
					continue
				}
				sPodinfo, err := s.podMap.GetPodInfo(sPod)
				for _, macvlan := range podinfo.MacvlanInterfaces {
					if !macvlan.CheckPolicyNetwork(policyNetworks) {
						continue
					}
					for _, sMacvlan := range sPodinfo.MacvlanInterfaces {
						if !sMacvlan.CheckPolicyNetwork(policyNetworks) {
							continue
						}
						for _, ip := range sMacvlan.IPs {
							writeLine(buf.ingressFrom, "-A", string(chainName),
								"-i", macvlan.InterfaceName, "-s", ip,
								"-j", "MARK", "--set-xmark", "0x20000/0x20000")
						}
					}
				}
			}
		} else if peer.IPBlock != nil {
			for _, except := range peer.IPBlock.Except {
				for _, macvlan := range podinfo.MacvlanInterfaces {
					if !macvlan.CheckPolicyNetwork(policyNetworks) {
						continue
					}
					writeLine(buf.ingressFrom, "-A", string(chainName),
						"-i", macvlan.InterfaceName, "-s", except, "-j", "DROP")
				}
			}
			for _, macvlan := range podinfo.MacvlanInterfaces {
				if !macvlan.CheckPolicyNetwork(policyNetworks) {
					continue
				}
				writeLine(buf.ingressFrom, "-A", string(chainName),
					"-i", macvlan.InterfaceName, "-s", peer.IPBlock.CIDR,
					"-j", "MARK", "--set-xmark", "0x20000/0x20000")
			}
		} else {
			klog.Errorf("unknown rule")
		}
	}
}

func (buf *iptableBuffer) renderEgress(s *Server, pod *v1.Pod, egresses []mvlanv1.MacvlanNetworkPolicyEgressRule, policyNetworks []string) {
	for n, egress := range egresses {
		writeLine(buf.policyIndex, "-A", macvlanEgressChain, "-j", "MARK", "--set-xmark 0x0/0x30000")
		buf.renderEgressPorts(s, pod, n, egress.Ports, policyNetworks)
		buf.renderEgressTo(s, pod, n, egress.To, policyNetworks)
		writeLine(buf.policyIndex, "-A", macvlanEgressChain, "-m", "mark", "--mark", "0x30000/0x30000", "-j", "RETURN")
	}
	writeLine(buf.policyIndex, "-A", macvlanEgressChain, "-j", "DROP")
}

func (buf *iptableBuffer) renderEgressPorts(s *Server, pod *v1.Pod, index int, ports []mvlanv1.MacvlanNetworkPolicyPort, policyNetworks []string) {
	chainName := utiliptables.Chain(fmt.Sprintf("MACVLAN-EGRESS-%d-PORTS", index))

	buf.activeChain[utiliptables.Chain(chainName)] = true
	// Create chain if not exists
	if chain, ok := buf.currentFilter[chainName]; ok {
		writeBytesLine(buf.filterChains, chain)
	} else {
		writeLine(buf.filterChains, utiliptables.MakeChainLine(chainName))
	}

	// Add jump from MACVLAN-EGRESS
	writeLine(buf.policyIndex, "-A", macvlanEgressChain, "-j", string(chainName))

	// Add skip rules if no ports
	if len(ports) == 0 {
		writeLine(buf.egressPorts, "-A", string(chainName),
			"-m", "comment", "--comment", "\"no egress ports, skipped\"",
			"-j", "MARK", "--set-xmark", "0x10000/0x10000")
		return
	}

	for _, port := range ports {
		proto := strings.ToLower(string(*port.Protocol))
		podinfo, err := s.podMap.GetPodInfo(pod)
		if err != nil {
			klog.Errorf("cannot get podinfo")
			continue
		}
		for _, macvlanIF := range podinfo.MacvlanInterfaces {
			if !macvlanIF.CheckPolicyNetwork(policyNetworks) {
				continue
			}
			writeLine(buf.egressPorts, "-A", string(chainName),
				"-m", "comment", "--comment", "\"comment\"", "-o", macvlanIF.InterfaceName,
				"-m", proto, "-p", proto, "--dport", port.Port.String(),
				"-j", "MARK", "--set-xmark", "0x10000/0x10000")
		}
	}
}

func (buf *iptableBuffer) renderEgressTo(s *Server, pod *v1.Pod, index int, to []mvlanv1.MacvlanNetworkPolicyPeer, policyNetworks []string) {
	chainName := utiliptables.Chain(fmt.Sprintf("MACVLAN-EGRESS-%d-TO", index))
	podinfo, err := s.podMap.GetPodInfo(pod)
	if err != nil {
		klog.Errorf("cannot get podinfo")
		return
	}

	buf.activeChain[utiliptables.Chain(chainName)] = true
	// Create chain if not exists
	if chain, ok := buf.currentFilter[chainName]; ok {
		writeBytesLine(buf.filterChains, chain)
	} else {
		writeLine(buf.filterChains, utiliptables.MakeChainLine(chainName))
	}

	// Add jump from MACVLAN-EGRESS
	writeLine(buf.policyIndex, "-A", macvlanEgressChain, "-j", string(chainName))

	// Add skip rules if no to
	if len(to) == 0 {
		writeLine(buf.egressTo, "-A", string(chainName),
			"-m", "comment", "--comment", "\"no egress to, skipped\"",
			"-j", "MARK", "--set-xmark", "0x20000/0x20000")
		return
	}

	for _, peer := range to {
		if peer.PodSelector != nil {
			podSelectorMap, err := metav1.LabelSelectorAsMap(peer.PodSelector)
			if err != nil {
				klog.Errorf("pod selector: %v", err)
				continue
			}
			podLabelSelector := labels.Set(podSelectorMap).AsSelectorPreValidated()
			pods, err := s.podLister.Pods(metav1.NamespaceAll).List(podLabelSelector)
			if err != nil {
				klog.Errorf("pod list failed:%v", err)
				continue
			}

			var nsSelector labels.Selector
			if peer.NamespaceSelector != nil {
				nsSelectorMap, err := metav1.LabelSelectorAsMap(peer.NamespaceSelector)
				if err != nil {
					klog.Errorf("namespace selector: %v", err)
					continue
				}
				nsSelector = labels.Set(nsSelectorMap).AsSelectorPreValidated()
			}
			s.namespaceMap.Update(s.nsChanges)

			for _, sPod := range pods {
				nsLabels, err := s.namespaceMap.GetNamespaceInfo(sPod.Namespace)
				if err != nil {
					klog.Errorf("cannot get namespace info: %v", err)
					continue
				}
				if nsSelector != nil && !nsSelector.Matches(labels.Set(nsLabels.Labels)) {
					continue
				}
				sPodinfo, err := s.podMap.GetPodInfo(sPod)
				for _, macvlan := range podinfo.MacvlanInterfaces {
					if !macvlan.CheckPolicyNetwork(policyNetworks) {
						continue
					}
					for _, sMacvlan := range sPodinfo.MacvlanInterfaces {
						if !sMacvlan.CheckPolicyNetwork(policyNetworks) {
							continue
						}
						for _, ip := range sMacvlan.IPs {
							writeLine(buf.egressTo, "-A", string(chainName),
								"-o", macvlan.InterfaceName, "-d", ip,
								"-j", "MARK", "--set-xmark", "0x20000/0x20000")
						}
					}
				}
			}
		} else if peer.IPBlock != nil {
			for _, except := range peer.IPBlock.Except {
				for _, macvlan := range podinfo.MacvlanInterfaces {
					if !macvlan.CheckPolicyNetwork(policyNetworks) {
						continue
					}
					writeLine(buf.egressTo, "-A", string(chainName),
						"-o", macvlan.InterfaceName, "-d", except, "-j", "DROP")
				}
			}
			for _, macvlan := range podinfo.MacvlanInterfaces {
				if !macvlan.CheckPolicyNetwork(policyNetworks) {
					continue
				}
				writeLine(buf.egressTo, "-A", string(chainName),
					"-o", macvlan.InterfaceName, "-d", peer.IPBlock.CIDR,
					"-j", "MARK", "--set-xmark", "0x20000/0x20000")
			}
		} else {
			klog.Errorf("unknown rule")
		}
	}
}

// Join all words with spaces, terminate with newline and write to buf.
func writeLine(buf *bytes.Buffer, words ...string) {
	// We avoid strings.Join for performance reasons.
	for i := range words {
		buf.WriteString(words[i])
		if i < len(words)-1 {
			buf.WriteByte(' ')
		} else {
			buf.WriteByte('\n')
		}
	}
}

func writeBytesLine(buf *bytes.Buffer, bytes []byte) {
	buf.Write(bytes)
	buf.WriteByte('\n')
}
