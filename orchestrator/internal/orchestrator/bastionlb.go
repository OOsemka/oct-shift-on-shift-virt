package orchestrator

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
)

// The bastion load balancer replaces the per-cluster HAProxy pods for new
// clusters (status.loadBalancer = "bastion").
//
// Why a VM and not pods: the bastion has its own routing table. Its external
// NIC sits on the selected VLAN network (Linux bridge or OVN localnet) with
// the API and Ingress addresses configured by cloud-init, and replies from
// those addresses leave through the VLAN gateway (policy routing). The
// HAProxy pods could not do that without NET_ADMIN: their default route
// pointed at the cluster network, so clients outside the VLAN never got an
// answer, and on Linux bridge networks they did not even get their static
// address (the NAD has no IPAM). The bastion is a VM, so VM->VM traffic to
// the masters' 22623/22624 is allowed and the MCS relay hop is not needed.
//
// The balancer is the same small Python TCP proxy as the old MCS relay
// (python3 ships in the cloud image; no package install, works disconnected).

const bastionExternalIface = "external"

// bastionLB is everything the bastion needs to front the nested cluster.
type bastionLB struct {
	APIIP        string
	IngressIP    string
	Gateway      string
	Prefix       int
	MAC          string
	Masters      []string
	IngressNodes []string
}

// useBastionLB reports whether this cluster's load balancer runs on the
// bastion. Clusters created before it existed keep their HAProxy pods.
func useBastionLB(nc *crd.NestedCluster) bool {
	return nc.Status.LoadBalancer == crd.LoadBalancerBastion
}

// externalMAC is a stable, locally administered unicast MAC for the bastion's
// external NIC, so cloud-init finds the right interface by MAC.
func externalMAC(cluster string) string {
	sum := sha256.Sum256([]byte("sos-bastion-external/" + cluster))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}

// newBastionLB builds the balancer settings from the cluster spec and the
// node addresses collected so far.
func newBastionLB(nc *crd.NestedCluster) (*bastionLB, error) {
	ea := nc.Spec.ExternalAccess
	lb := &bastionLB{
		APIIP:     strings.TrimSpace(ea.APIIP),
		IngressIP: strings.TrimSpace(ea.IngressIP),
		Gateway:   strings.TrimSpace(ea.Gateway),
		Prefix:    ea.Prefix(),
		MAC:       externalMAC(nc.Spec.ClusterName),
	}
	for _, n := range nc.Status.Nodes {
		if n.IP == "" {
			continue
		}
		lb.IngressNodes = append(lb.IngressNodes, n.IP)
		if n.Role == "" || n.Role == "master" {
			lb.Masters = append(lb.Masters, n.IP)
		}
	}
	switch {
	case lb.APIIP == "" || lb.IngressIP == "":
		return nil, fmt.Errorf("external API/Ingress IP missing from the spec")
	case lb.Gateway == "":
		return nil, fmt.Errorf("external gateway missing from the spec")
	case len(lb.Masters) == 0:
		return nil, fmt.Errorf("no master IPs in status for the load balancer backends")
	}
	return lb, nil
}

// configJSON is /etc/sos-lb.json: listen port -> backends. 12262/12264 keep
// the old relay ports alive for anything still pointed at them.
func (b *bastionLB) configJSON() string {
	front := map[string][][2]interface{}{}
	add := func(port int, nodes []string, backendPort int) {
		for _, n := range nodes {
			front[fmt.Sprint(port)] = append(front[fmt.Sprint(port)], [2]interface{}{n, backendPort})
		}
	}
	add(6443, b.Masters, 6443)
	add(22623, b.Masters, 22623)
	add(22624, b.Masters, 22624)
	add(12262, b.Masters, 22623)
	add(12264, b.Masters, 22624)
	add(443, b.IngressNodes, 443)
	add(80, b.IngressNodes, 80)
	keys := make([]string, 0, len(front))
	for k := range front {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make([]string, 0, len(keys))
	for _, k := range keys {
		v, _ := json.Marshal(front[k])
		ordered = append(ordered, fmt.Sprintf("%q: %s", k, v))
	}
	return "{\"frontends\": {" + strings.Join(ordered, ", ") + "}}\n"
}

// externalEnv is /etc/sos-external.env for sos-external.sh.
func (b *bastionLB) externalEnv() string {
	return fmt.Sprintf("MAC=%s\nAPI_IP=%s\nINGRESS_IP=%s\nPREFIX=%d\nGATEWAY=%s\n",
		b.MAC, b.APIIP, b.IngressIP, b.Prefix, b.Gateway)
}

// writeFiles renders the cloud-init write_files entries for the balancer and
// the external NIC.
func (b *bastionLB) writeFiles() string {
	return fmt.Sprintf(`  - path: /opt/sos-lb/lb.py
    permissions: '0755'
    content: |
%s  - path: /etc/sos-lb.json
    permissions: '0644'
    content: |
%s  - path: /etc/systemd/system/sos-lb.service
    permissions: '0644'
    content: |
      [Unit]
      Description=SOS load balancer (API 6443, MCS 22623/22624, Ingress 443/80)
      After=network-online.target
      [Service]
      ExecStart=/usr/bin/python3 /opt/sos-lb/lb.py /etc/sos-lb.json
      Restart=always
      RestartSec=3
      LimitNOFILE=65536
      [Install]
      WantedBy=multi-user.target
  - path: /usr/local/sbin/sos-external.sh
    permissions: '0755'
    content: |
%s  - path: /etc/sos-external.env
    permissions: '0644'
    content: |
%s  - path: /etc/systemd/system/sos-external.service
    permissions: '0644'
    content: |
      [Unit]
      Description=SOS external VLAN addresses and return routing
      After=NetworkManager.service
      Wants=NetworkManager.service
      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStart=/usr/local/sbin/sos-external.sh
      Restart=on-failure
      RestartSec=10
      [Install]
      WantedBy=multi-user.target
`, indentBlock(bastionLBScript, "      "), indentBlock(b.configJSON(), "      "),
		indentBlock(sosExternalScript, "      "), indentBlock(b.externalEnv(), "      "))
}

// sosExternalScript configures the external NIC (found by its pinned MAC)
// with the API and Ingress addresses, and makes replies from those addresses
// use the VLAN gateway (table 100) while everything else keeps the cluster
// network's default route. NetworkManager persists it across reboots.
const sosExternalScript = `#!/bin/bash
set -euo pipefail
. /etc/sos-external.env
IF=""
for i in $(seq 1 60); do
    IF=$(ip -o link | awk -v m="$(echo "$MAC" | tr A-F a-f)" 'tolower($0) ~ m {sub(":$","",$2); sub("@.*","",$2); print $2; exit}')
    [ -n "$IF" ] && break
    sleep 2
done
[ -n "$IF" ] || { echo "no interface with MAC $MAC"; exit 1; }
for c in $(nmcli -g UUID,DEVICE con show | awk -F: -v d="$IF" '$2==d{print $1}'); do nmcli con delete "$c" || true; done
nmcli con delete sos-external >/dev/null 2>&1 || true
nmcli con add type ethernet ifname "$IF" con-name sos-external \
    ipv4.method manual ipv4.addresses "$API_IP/$PREFIX,$INGRESS_IP/$PREFIX" \
    ipv4.never-default yes \
    ipv4.routes "0.0.0.0/0 $GATEWAY table=100" \
    ipv4.routing-rules "priority 100 from $API_IP/32 table 100, priority 101 from $INGRESS_IP/32 table 100" \
    ipv6.method disabled connection.autoconnect yes
nmcli con up sos-external
sysctl -w net.ipv4.conf.all.rp_filter=2 net.ipv4.conf.default.rp_filter=2 >/dev/null
printf 'net.ipv4.conf.all.rp_filter = 2\nnet.ipv4.conf.default.rp_filter = 2\n' > /etc/sysctl.d/90-sos-external.conf
echo "external NIC $IF: $API_IP/$PREFIX, $INGRESS_IP/$PREFIX, replies via $GATEWAY"
`

// bastionLBScript is a threaded TCP proxy. Each frontend round-robins its
// backends and, like the HAProxy config it replaces (retries + redispatch,
// no health checks), tries the next backend when a connect is refused or
// times out, so a master that is still booting costs milliseconds.
const bastionLBScript = `import itertools, json, socket, sys, threading

CFG = json.load(open(sys.argv[1]))
FRONTENDS = {int(p): [(h, int(bp)) for h, bp in v] for p, v in CFG["frontends"].items() if v}
COUNTERS = {p: itertools.count() for p in FRONTENDS}
LOCK = threading.Lock()


def pump(src, dst):
    try:
        while True:
            data = src.recv(65536)
            if not data:
                break
            dst.sendall(data)
    except OSError:
        pass
    finally:
        for s in (src, dst):
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass


def connect(port):
    backends = FRONTENDS[port]
    with LOCK:
        start = next(COUNTERS[port])
    for i in range(len(backends)):
        target = backends[(start + i) % len(backends)]
        try:
            remote = socket.create_connection(target, timeout=2)
        except OSError:
            continue
        # The connect timeout must not stay on the socket: API watches and
        # other long-lived streams idle for minutes.
        remote.settimeout(None)
        remote.setsockopt(socket.SOL_SOCKET, socket.SO_KEEPALIVE, 1)
        return remote
    return None


def handle(client, port):
    client.setsockopt(socket.SOL_SOCKET, socket.SO_KEEPALIVE, 1)
    remote = connect(port)
    if remote is None:
        client.close()
        return
    t1 = threading.Thread(target=pump, args=(client, remote), daemon=True)
    t2 = threading.Thread(target=pump, args=(remote, client), daemon=True)
    t1.start(); t2.start(); t1.join(); t2.join()
    client.close(); remote.close()


def serve(port):
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("0.0.0.0", port))
    srv.listen(512)
    while True:
        client, _ = srv.accept()
        threading.Thread(target=handle, args=(client, port), daemon=True).start()


for p in FRONTENDS:
    threading.Thread(target=serve, args=(p,), daemon=True).start()
threading.Event().wait()
`
