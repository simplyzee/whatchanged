package cli

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/simplyzee/whatchanged/internal/client"
)

// connFlags decide how the CLI reaches the API.
type connFlags struct {
	server      string
	tok         string
	portForward bool
	installNS   string
	service     string
	servicePort string
	kubeContext string
	kubeconfig  string
}

func (c *connFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.server, "server", os.Getenv("WHATCHANGED_URL"), "whatchanged API address (default $WHATCHANGED_URL or http://localhost:8080)")
	fs.StringVar(&c.tok, "token", "", "bearer token for writes (default $WHATCHANGED_TOKEN)")
	fs.BoolVar(&c.portForward, "port-forward", Plugin && os.Getenv("WHATCHANGED_URL") == "", "reach the in-cluster service with kubectl port-forward")
	fs.StringVar(&c.installNS, "install-namespace", envOr("WHATCHANGED_NAMESPACE", "whatchanged"), "namespace whatchanged runs in (for --port-forward)")
	fs.StringVar(&c.service, "service", envOr("WHATCHANGED_SERVICE", "whatchanged"), "service name (for --port-forward)")
	fs.StringVar(&c.servicePort, "service-port", envOr("WHATCHANGED_SERVICE_PORT", "8080"), "service port (for --port-forward)")
	fs.StringVar(&c.kubeContext, "context", "", "kubeconfig context (for --port-forward)")
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig file (for --port-forward)")
}

func (c *connFlags) token() string {
	if c.tok != "" {
		return c.tok
	}
	return os.Getenv("WHATCHANGED_TOKEN")
}

// connect returns a client and a cleanup function.
func (c *connFlags) connect(stderr io.Writer) (*client.Client, func(), error) {
	if c.server != "" && !c.portForward {
		return client.New(c.server, c.token()), func() {}, nil
	}
	if !c.portForward {
		return client.New("http://localhost:8080", c.token()), func() {}, nil
	}
	addr, stop, err := portForward(c)
	if err != nil {
		return nil, nil, err
	}
	return client.New("http://"+addr, c.token()), stop, nil
}

var forwardingRE = regexp.MustCompile(`Forwarding from (127\.0\.0\.1:\d+)`)

func kubectlBin() string { return envOr("KUBECTL", "kubectl") }

// portForward starts `kubectl port-forward` to the whatchanged service and
// returns the local address once the tunnel is up.
func portForward(c *connFlags) (string, func(), error) {
	args := []string{"port-forward", "--address", "127.0.0.1", "-n", c.installNS, "svc/" + c.service, ":" + c.servicePort}
	if c.kubeContext != "" {
		args = append(args, "--context", c.kubeContext)
	}
	if c.kubeconfig != "" {
		args = append(args, "--kubeconfig", c.kubeconfig)
	}
	cmd := exec.Command(kubectlBin(), args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start kubectl port-forward: %w (is kubectl on your PATH?)", err)
	}
	var once sync.Once
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if m := forwardingRE.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case found <- m[1]:
				default:
				}
			}
		}
	}()
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	select {
	case addr := <-found:
		stop := func() {
			once.Do(func() {
				_ = cmd.Process.Kill()
				<-exited
			})
		}
		return addr, stop, nil
	case <-exited:
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = "kubectl port-forward exited"
		}
		return "", nil, fmt.Errorf("port-forward to svc/%s in namespace %s failed: %s\n(is whatchanged installed? set --install-namespace/--service, or --server)", c.service, c.installNS, msg)
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		return "", nil, errors.New("timed out waiting for kubectl port-forward")
	}
}
