package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestBastionLBScriptPython runs the health-check logic of lb.py under the
// local python3 (stdlib unittest). The bastion runs Python 3.9; keep lb.py
// free of newer syntax.
func TestBastionLBScriptPython(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lb.py"), []byte(bastionLBScript), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test_lb.py"), []byte(lbPyTest), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, "-I", filepath.Join(dir, "test_lb.py"))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lb.py tests failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

const lbPyTest = `import http.server, os, socket, ssl, sys, threading, time, unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import lb  # noqa: E402

CHECKS = {
    "api": {"scheme": "https", "port": 6443, "path": "/readyz", "interval": 5, "timeout": 3, "rise": 2, "fall": 3},
    "ingress": {"scheme": "http", "port": 1936, "path": "/healthz/ready", "interval": 10, "timeout": 5, "rise": 2, "fall": 2},
}
NODES = ["10.0.0.1", "10.0.0.2", "10.0.0.3"]


def cfg(checks=True):
    c = {"frontends": {
        "6443": [[n, 6443] for n in NODES],
        "443": [[n, 443] for n in NODES],
        "80": [[n, 80] for n in NODES],
    }}
    if checks:
        c["checks"] = CHECKS
        c["frontend_checks"] = {"6443": "api", "443": "ingress", "80": "ingress"}
    return c


class HealthTest(unittest.TestCase):
    def test_initial_healthy_and_rise_fall(self):
        h = lb.Health("api", "10.0.0.1", rise=2, fall=3)
        self.assertTrue(h.healthy)
        self.assertIsNone(h.record(False, "HTTP 503"))
        self.assertIsNone(h.record(False, "HTTP 503"))
        self.assertTrue(h.healthy)
        self.assertEqual(h.record(False, "HTTP 503"), "api 10.0.0.1 DOWN (3 failures: HTTP 503)")
        self.assertFalse(h.healthy)
        self.assertIsNone(h.record(False, "HTTP 503"))  # no repeated transition
        self.assertIsNone(h.record(True))
        self.assertFalse(h.healthy)
        self.assertEqual(h.record(True), "api 10.0.0.1 UP (2 successes)")
        self.assertTrue(h.healthy)

    def test_success_resets_failures(self):
        h = lb.Health("api", "h", rise=2, fall=3)
        for ok in (False, False, True, False, False):
            h.record(ok)
        self.assertTrue(h.healthy)


class BalancerTest(unittest.TestCase):
    def down(self, b, check, host):
        st = b.health[(check, host)]
        while st.healthy:
            st.record(False, "test")

    def test_no_checks_behaves_as_before(self):
        b = lb.Balancer(cfg(checks=False))
        self.assertEqual(b.health, {})
        self.assertEqual(b.start_probers(threading.Event()), [])
        self.assertEqual(sorted(b.candidates(6443)), sorted((n, 6443) for n in NODES))

    def test_state_per_check_and_host(self):
        b = lb.Balancer(cfg())
        self.assertEqual(len(b.health), 6)  # api x3 + ingress x3, 80/443 shared
        self.down(b, "ingress", "10.0.0.2")
        self.assertFalse(b.is_healthy(443, "10.0.0.2"))
        self.assertFalse(b.is_healthy(80, "10.0.0.2"))
        self.assertTrue(b.is_healthy(6443, "10.0.0.2"))

    def test_round_robin_over_healthy_only(self):
        b = lb.Balancer(cfg())
        self.down(b, "api", "10.0.0.2")
        firsts = [b.candidates(6443)[0][0] for _ in range(6)]
        self.assertNotIn("10.0.0.2", firsts)
        self.assertEqual(set(firsts), {"10.0.0.1", "10.0.0.3"})
        self.assertNotEqual(firsts[0], firsts[1])
        # the unhealthy one stays as the very last resort
        self.assertEqual(b.candidates(6443)[-1], ("10.0.0.2", 6443))

    def test_fail_open_when_all_down(self):
        b = lb.Balancer(cfg())
        for n in NODES:
            self.down(b, "ingress", n)
        for port in (80, 443):
            c = b.candidates(port)
            self.assertEqual(sorted(c), sorted((n, port) for n in NODES))
        firsts = {b.candidates(443)[0][0] for _ in range(3)}
        self.assertEqual(firsts, set(NODES))

    def test_fail_open_logged_once_per_transition(self):
        b = lb.Balancer(cfg())
        lines = []
        orig, lb.log = lb.log, lines.append
        try:
            for n in NODES:
                self.down(b, "api", n)
            for _ in range(5):
                b.candidates(6443)
            b.health[("api", "10.0.0.1")].record(True)
            b.health[("api", "10.0.0.1")].record(True)
            for _ in range(5):
                b.candidates(6443)
        finally:
            lb.log = orig
        self.assertEqual(lines, ["frontend 6443: no healthy backend, using all (fail-open)",
                                 "frontend 6443: healthy backends again"])

    def test_listen_binds_immediately(self):
        b = lb.Balancer(cfg())
        srv = b.listen(0, "127.0.0.1")
        try:
            socket.create_connection(srv.getsockname(), timeout=1).close()
        finally:
            srv.close()


class Handler(http.server.BaseHTTPRequestHandler):
    status = 200

    def do_GET(self):
        self.send_response(Handler.status if self.path == "/healthz/ready" else 404)
        self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, *args):
        pass


class Server(http.server.ThreadingHTTPServer):
    # Do not wait for handler threads on close: the TLS-against-HTTP probe
    # leaves one blocked reading a request line.
    daemon_threads = True
    block_on_close = False


class ProbeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = Server(("127.0.0.1", 0), Handler)
        cls.port = cls.srv.server_address[1]
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()
        cls.srv.server_close()

    def check(self, **kw):
        c = {"scheme": "http", "port": self.port, "path": "/healthz/ready", "interval": 0.05, "timeout": 1, "rise": 2, "fall": 2}
        c.update(kw)
        return c

    def test_http_200_and_503(self):
        Handler.status = 200
        self.assertEqual(lb.probe(self.check(), "127.0.0.1"), (True, "HTTP 200"))
        Handler.status = 503
        self.assertEqual(lb.probe(self.check(), "127.0.0.1"), (False, "HTTP 503"))
        Handler.status = 200
        self.assertFalse(lb.probe(self.check(path="/other"), "127.0.0.1")[0])

    def test_connection_refused_and_tls_errors_fail(self):
        s = socket.socket()
        s.bind(("127.0.0.1", 0))
        closed = s.getsockname()[1]
        s.close()
        self.assertFalse(lb.probe(self.check(port=closed), "127.0.0.1")[0])
        # HTTPS against a plain HTTP server: handshake fails = unhealthy
        self.assertFalse(lb.probe(self.check(scheme="https"), "127.0.0.1")[0])

    def test_connect_skips_refusing_backend(self):
        s = socket.socket()
        s.bind(("127.0.0.1", 0))
        closed = s.getsockname()[1]
        s.close()
        b = lb.Balancer({"frontends": {"8443": [["127.0.0.1", closed], ["127.0.0.1", self.port]]}})
        for _ in range(2):  # whichever backend round-robin starts with
            remote = b.connect(8443)
            self.assertIsNotNone(remote)
            self.assertEqual(remote.getpeername()[1], self.port)
            self.assertIsNone(remote.gettimeout())
            remote.close()

    def test_insecure_context(self):
        ctx = lb.insecure_context()
        self.assertFalse(ctx.check_hostname)
        self.assertEqual(ctx.verify_mode, ssl.CERT_NONE)

    def test_prober_thread_transitions(self):
        c = {"frontends": {"443": [["127.0.0.1", 443]], "80": [["127.0.0.1", 80]]},
             "checks": {"ingress": self.check()},
             "frontend_checks": {"443": "ingress", "80": "ingress"}}
        b = lb.Balancer(c)
        stop = threading.Event()
        Handler.status = 503
        threads = b.start_probers(stop)
        self.assertEqual(len(threads), 1)  # one prober for the shared state
        try:
            self.assertTrue(wait(lambda: not b.is_healthy(443, "127.0.0.1")))
            self.assertFalse(b.is_healthy(80, "127.0.0.1"))
            Handler.status = 200
            self.assertTrue(wait(lambda: b.is_healthy(80, "127.0.0.1")))
        finally:
            stop.set()
            Handler.status = 200
            for t in threads:
                t.join(2)


def wait(cond, timeout=3.0):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        if cond():
            return True
        time.sleep(0.01)
    return False


if __name__ == "__main__":
    unittest.main(verbosity=2)
`
