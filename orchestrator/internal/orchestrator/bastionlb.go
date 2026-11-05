package orchestrator

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
// (python3 ships in the cloud image; no package install, works disconnected),
// plus HTTP(S) readiness probes per backend (API /readyz, MCS /healthz,
// router /healthz/ready on 1936) that take unready backends out of rotation.
// It fails open: with no healthy backend it uses all of them.
//
// lb.py and /etc/sos-lb.json reach the bastion only through cloud-init when
// the VM is created; existing bastions keep the version they booted with.

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

// lbCheck is one HTTP(S) readiness probe in /etc/sos-lb.json. Timings are in
// seconds. A backend goes DOWN after Fall consecutive failures and back UP
// after Rise consecutive successes; only HTTP 200 counts as a success.
type lbCheck struct {
	Scheme   string `json:"scheme"`
	Port     int    `json:"port"`
	Path     string `json:"path"`
	Interval int    `json:"interval"`
	Timeout  int    `json:"timeout"`
	Rise     int    `json:"rise"`
	Fall     int    `json:"fall"`
}

// Probes follow the OpenShift 4.22 docs. "Installing on any platform", load
// balancing requirements: the API balancer must use /readyz and take an
// unready instance out within a maximum of 30 seconds, "probing every 5 or 10
// seconds, with two successful requests to become healthy and three to
// become unhealthy". "Configuring a user-managed load balancer":
// HTTPS:22623/healthz for MCS, HTTP:1936/healthz/ready for ingress (healthy
// 2, unhealthy 2, timeout 5, interval 10).
//
// API/MCS use a 5 s timeout: a busy bootstrap kube-apiserver can be slow on
// the TLS handshake. Worst-case removal is 3*5+5 = 20 s. 22624 is the MCS's
// plain-HTTP server with the same /healthz handler, so it has its own check.
var lbChecks = map[string]lbCheck{
	"api":      {Scheme: "https", Port: 6443, Path: "/readyz", Interval: 5, Timeout: 5, Rise: 2, Fall: 3},
	"mcs":      {Scheme: "https", Port: 22623, Path: "/healthz", Interval: 5, Timeout: 5, Rise: 2, Fall: 3},
	"mcs-http": {Scheme: "http", Port: 22624, Path: "/healthz", Interval: 5, Timeout: 5, Rise: 2, Fall: 3},
	"ingress":  {Scheme: "http", Port: 1936, Path: "/healthz/ready", Interval: 10, Timeout: 5, Rise: 2, Fall: 2},
}

// lbConfig is the shape of /etc/sos-lb.json. "frontends" (listen port ->
// [[host, port], ...]) is unchanged from the first version; "checks" and
// "frontend_checks" (listen port -> check name) add health checking. lb.py
// without "checks" behaves as before (no probing).
type lbConfig struct {
	Checks         map[string]lbCheck          `json:"checks"`
	Frontends      map[string][][2]interface{} `json:"frontends"`
	FrontendChecks map[string]string           `json:"frontend_checks"`
}

// configJSON is /etc/sos-lb.json: listen port -> backends and the check that
// decides which of them are healthy. 12262/12264 keep the old relay ports
// alive for anything still pointed at them: 12262 shares the HTTPS MCS check
// with 22623, 12264 the HTTP one with 22624. 80 and 443 share the router
// check.
func (b *bastionLB) configJSON() string {
	cfg := lbConfig{
		Checks:         lbChecks,
		Frontends:      map[string][][2]interface{}{},
		FrontendChecks: map[string]string{},
	}
	add := func(port int, nodes []string, backendPort int, check string) {
		key := fmt.Sprint(port)
		for _, n := range nodes {
			cfg.Frontends[key] = append(cfg.Frontends[key], [2]interface{}{n, backendPort})
		}
		cfg.FrontendChecks[key] = check
	}
	add(6443, b.Masters, 6443, "api")
	add(22623, b.Masters, 22623, "mcs")
	add(22624, b.Masters, 22624, "mcs-http")
	add(12262, b.Masters, 22623, "mcs")
	add(12264, b.Masters, 22624, "mcs-http")
	add(443, b.IngressNodes, 443, "ingress")
	add(80, b.IngressNodes, 80, "ingress")
	// encoding/json sorts map keys, so the output is stable.
	out, _ := json.Marshal(cfg)
	return string(out) + "\n"
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
      Description=SOS load balancer (API 6443, MCS 22623/22624, Ingress 443/80; HTTP(S) health checks)
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

// bastionLBScript is a threaded TCP proxy with HTTP(S) readiness probes.
//
// Forwarding is plain L4: bytes are pumped both ways, TLS is never
// terminated. Each frontend round-robins its backends and, like the HAProxy
// config it replaces (retries + redispatch), tries the next backend when a
// connect is refused or times out, so a master that is still booting costs
// milliseconds.
//
// Health checks are separate from forwarding: one prober thread per (check,
// backend host) runs the check from "checks" (HTTPS without certificate
// verification, or HTTP; only 200 is healthy) and flips the backend DOWN/UP
// with fall/rise counters. State is per (check, host), so 80/443 share the
// router state, 22623/12262 the HTTPS MCS state and 22624/12264 the HTTP MCS
// state. A backend is healthy until its probes say otherwise, so a fresh
// bastion forwards at once. Only transitions are logged: on a compact cluster
// one node fails 1936 for good (2 router replicas on 3 nodes) and must not
// log every cycle. No master is assumed to be the bootstrap/rendezvous node;
// every master is probed the same way. Fail-open: when no backend of a frontend is healthy, all of them are
// used (the agent-based install brings the control plane and routers up
// gradually; the balancer must never block it). Healthy backends are tried
// first (round-robin), the others only after every healthy one refused the
// connect. Without "checks" in the config nothing is probed and every
// backend is always used, as in the first version.
//
// Python 3.9 (CentOS Stream 9), stdlib only. The startup is guarded by
// __main__ so bastionlb_py_test.go can import it.
const bastionLBScript = `import http.client, itertools, json, socket, ssl, sys, threading, time


def log(msg):
    # Never raise: a closed stdout (journald restart) must not kill a
    # connection handler or a prober.
    try:
        print(msg, flush=True)
    except (OSError, ValueError):
        pass


def insecure_context():
    # Backends are reached by IP and serve self-signed or internal-CA
    # certificates: probes check readiness, not identity. No client cert.
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx


def probe(check, host):
    """One HTTP(S) GET. Returns (ok, detail); only HTTP 200 is ok.

    The socket timeout bounds each operation; a watchdog shuts the socket
    down at the overall deadline, so a backend trickling bytes cannot make
    one probe last longer than "timeout"."""
    timeout = float(check.get("timeout", 3))
    port = int(check["port"])
    expired = threading.Event()
    conn = None

    def kill():
        expired.set()
        sock = getattr(conn, "sock", None)
        if sock is not None:
            try:
                sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass

    watchdog = threading.Timer(timeout, kill)
    watchdog.daemon = True
    try:
        if check.get("scheme", "http") == "https":
            conn = http.client.HTTPSConnection(host, port, timeout=timeout, context=insecure_context())
        else:
            conn = http.client.HTTPConnection(host, port, timeout=timeout)
        watchdog.start()
        conn.request("GET", check.get("path", "/"), headers={"Connection": "close"})
        resp = conn.getresponse()
        resp.read(4096)
        if expired.is_set():
            return False, "timeout after %gs" % timeout
        if resp.status == 200:
            return True, "HTTP 200"
        return False, "HTTP %d" % resp.status
    except Exception as e:  # connection errors, timeouts, TLS errors
        if expired.is_set():
            return False, "timeout after %gs" % timeout
        return False, "%s: %s" % (type(e).__name__, e)
    finally:
        watchdog.cancel()
        if conn is not None:
            conn.close()


class Health:
    """Rise/fall state of one backend host for one check."""

    def __init__(self, check_name, host, rise, fall):
        self.check_name = check_name
        self.host = host
        self.rise = max(1, int(rise))
        self.fall = max(1, int(fall))
        self.healthy = True  # until the probes say otherwise
        self.successes = 0
        self.failures = 0
        self.lock = threading.Lock()

    def record(self, ok, detail=""):
        """Apply one probe result. Returns the log line of a transition, or None."""
        with self.lock:
            if ok:
                self.failures = 0
                self.successes += 1
                if not self.healthy and self.successes >= self.rise:
                    self.healthy = True
                    return "%s %s UP (%d successes)" % (self.check_name, self.host, self.successes)
            else:
                self.successes = 0
                self.failures += 1
                if self.healthy and self.failures >= self.fall:
                    self.healthy = False
                    return "%s %s DOWN (%d failures: %s)" % (self.check_name, self.host, self.failures, detail)
        return None


class Balancer:
    def __init__(self, cfg, probe_fn=probe):
        self.frontends = {int(p): [(h, int(bp)) for h, bp in v]
                          for p, v in cfg.get("frontends", {}).items() if v}
        self.counters = {p: itertools.count() for p in self.frontends}
        self.lock = threading.Lock()
        self.fail_open = set()  # listen ports currently using all backends
        self.checks = cfg.get("checks") or {}
        self.probe_fn = probe_fn
        # listen port -> check name, only for checks that exist.
        self.frontend_checks = {}
        if self.checks:
            for p, name in (cfg.get("frontend_checks") or {}).items():
                if name in self.checks and int(p) in self.frontends:
                    self.frontend_checks[int(p)] = name
        # (check name, host) -> Health: one entry per host, whatever the port.
        self.health = {}
        for p, name in self.frontend_checks.items():
            c = self.checks[name]
            for host, _ in self.frontends[p]:
                if (name, host) not in self.health:
                    self.health[(name, host)] = Health(name, host, c.get("rise", 2), c.get("fall", 3))

    def is_healthy(self, port, host):
        name = self.frontend_checks.get(port)
        if name is None:
            return True
        return self.health[(name, host)].healthy

    def note_fail_open(self, port, on):
        """Log once when a frontend enters or leaves fail-open, not per
        connection. The line is written under the lock, so the last journal
        line always matches the current state."""
        with self.lock:
            if (port in self.fail_open) == on:
                return
            if on:
                self.fail_open.add(port)
                log("frontend %d: no healthy backend, using all (fail-open)" % port)
            else:
                self.fail_open.discard(port)
                log("frontend %d: healthy backends again" % port)

    def candidates(self, port):
        """Backends in the order to try: round-robin over the healthy ones,
        then the rest as a last resort. All down = all used (fail-open)."""
        backends = self.frontends[port]
        with self.lock:
            start = next(self.counters[port])
        healthy = [b for b in backends if self.is_healthy(port, b[0])]
        others = [b for b in backends if b not in healthy]
        self.note_fail_open(port, not healthy)
        if not healthy:
            healthy, others = backends, []
        n = len(healthy)
        return [healthy[(start + i) % n] for i in range(n)] + others

    def connect(self, port):
        for target in self.candidates(port):
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

    def prober(self, name, host, stop):
        """Probe one host for one check until stop is set."""
        c = self.checks[name]
        state = self.health[(name, host)]
        interval = float(c.get("interval", 5))
        while not stop.is_set():
            began = time.monotonic()
            try:
                ok, detail = self.probe_fn(c, host)
                msg = state.record(ok, detail)
                if msg:
                    log(msg)
            except Exception as e:
                # A dead prober would freeze this backend's state for good.
                log("%s %s probe error: %s: %s" % (name, host, type(e).__name__, e))
            stop.wait(max(0.0, interval - (time.monotonic() - began)))

    def start_probers(self, stop):
        """One daemon thread per (check, host): bounded by checks x nodes."""
        threads = []
        for name, host in sorted(self.health):
            t = threading.Thread(target=self.prober, args=(name, host, stop), daemon=True)
            t.start()
            threads.append(t)
        return threads

    def handle(self, client, port):
        client.setsockopt(socket.SOL_SOCKET, socket.SO_KEEPALIVE, 1)
        remote = self.connect(port)
        if remote is None:
            client.close()
            return
        t1 = threading.Thread(target=pump, args=(client, remote), daemon=True)
        t2 = threading.Thread(target=pump, args=(remote, client), daemon=True)
        t1.start(); t2.start(); t1.join(); t2.join()
        client.close(); remote.close()

    def listen(self, port, addr="0.0.0.0"):
        srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        srv.bind((addr, port))
        srv.listen(512)
        return srv

    def serve(self, srv, port):
        while True:
            # Out of file descriptors or threads (EMFILE, "can't start new
            # thread") must not kill the listener while the port stays bound.
            try:
                client, _ = srv.accept()
            except OSError:
                time.sleep(0.1)
                continue
            try:
                threading.Thread(target=self.handle, args=(client, port), daemon=True).start()
            except RuntimeError:
                client.close()
                time.sleep(0.1)

    def run(self):
        # Bind every listener before the first probe: the DeployDNS job checks
        # that the ports listen before any node has booted.
        servers = [(self.listen(p), p) for p in self.frontends]
        for srv, p in servers:
            threading.Thread(target=self.serve, args=(srv, p), daemon=True).start()
        self.start_probers(threading.Event())
        threading.Event().wait()


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


if __name__ == "__main__":
    with open(sys.argv[1]) as f:
        Balancer(json.load(f)).run()
`
