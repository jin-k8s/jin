package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jin-k8s/jin/internal/audit"
	"github.com/jin-k8s/jin/internal/auth"
	"github.com/jin-k8s/jin/internal/clusters"
	"github.com/jin-k8s/jin/internal/config"
	"github.com/jin-k8s/jin/internal/secrets"
	"github.com/jin-k8s/jin/internal/server"
	"github.com/jin-k8s/jin/internal/upgrade"
)

type serverOptions struct {
	listen   string
	open     bool
	operator string
}

func newServerCmd(g *globalOptions) *cobra.Command {
	o := &serverOptions{}
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the Jin web UI and upgrade engine",
		Long: "Starts the web UI and API. Plan upgrades, review findings, approve each hop and follow\n" +
			"upgrades live. Upgrades in progress survive restarts and resume where they stopped.\n\n" +
			"The server binds to localhost by default and authenticates with a token stored in $JIN_HOME.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runServer(cmd, g, o) },
	}
	f := cmd.Flags()
	f.StringVar(&o.listen, "listen", "127.0.0.1:7420", "address to listen on")
	f.BoolVar(&o.open, "open", false, "open the UI in the default browser")
	f.StringVar(&o.operator, "operator", defaultOperator(), "name recorded for token sign-ins (SSO users are recorded by email)")
	return cmd
}

func runServer(cmd *cobra.Command, g *globalOptions, o *serverOptions) error {
	env, err := g.env()
	if err != nil {
		return err
	}
	home, err := jinHome()
	if err != nil {
		return err
	}
	token, err := loadOrCreateToken(filepath.Join(home, "server-token"))
	if err != nil {
		return err
	}

	host, _, err := net.SplitHostPort(o.listen)
	if err != nil {
		return fmt.Errorf("invalid --listen: %w", err)
	}
	loopback := host == "localhost" || net.ParseIP(host).IsLoopback()

	cfg, err := config.Load(filepath.Join(home, "config.yaml"))
	if err != nil {
		return err
	}
	key, err := auth.LoadKey(filepath.Join(home, "session-key"))
	if err != nil {
		return err
	}
	authn, err := auth.New(cmd.Context(), cfg, key, token, o.operator)
	if err != nil {
		return err
	}
	auditLog, err := audit.Open(filepath.Join(home, "audit.jsonl"))
	if err != nil {
		return err
	}
	if ok, at, err := auditLog.Verify(); err == nil && !ok {
		fmt.Fprintf(cmd.ErrOrStderr(), "  WARNING: audit log hash chain is broken at entry %d; it may have been edited.\n", at)
	}
	env.Settings = clusters.NewStore(filepath.Join(home, "clusters.json"))
	if env.Secrets, err = secrets.Open(filepath.Join(home, "secrets.json"), filepath.Join(home, "secrets.key")); err != nil {
		return err
	}

	engine := upgrade.NewEngine(upgrade.NewStore(filepath.Join(home, "upgrades")), env.Connector())
	if err := engine.Resume(); err != nil {
		return fmt.Errorf("resume upgrades: %w", err)
	}
	defer engine.Shutdown()

	srv, err := server.New(server.Config{Env: env, Engine: engine, Auth: authn, Cfg: cfg, Audit: auditLog, LoopbackOnly: loopback})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	base := fmt.Sprintf("http://%s/", displayAddr(ln.Addr().String()))
	url := base
	out := cmd.ErrOrStderr()
	fmt.Fprintf(out, "\n  \033[1mjin\033[0m server %s\n\n", "ready")
	if authn.TokenLoginEnabled() {
		url = base + "?token=" + token
		fmt.Fprintf(out, "  Open:      %s\n", url)
		fmt.Fprintf(out, "  Operator:  %s (token sign-in is an admin session)\n", o.operator)
	} else {
		fmt.Fprintf(out, "  Open:      %s\n", base)
	}
	if authn.SSOEnabled() {
		fmt.Fprintf(out, "  SSO:       %s\n", cfg.Auth.OIDC.Issuer)
	}
	fmt.Fprintf(out, "  Policies:  %d approval polic(ies) from config.yaml\n", len(cfg.Policies))
	fmt.Fprintf(out, "  Data:      %s\n\n", home)
	if !loopback {
		fmt.Fprintf(out, "  WARNING: listening on a non-loopback address. The API can change clusters;\n"+
			"  put it behind TLS and an authenticating proxy.\n\n")
	}
	if o.open {
		openBrowser(url)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		fmt.Fprintln(out, "\n  Shutting down. Running upgrades pause and resume on next start.")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}
	return nil
}

func jinHome() (string, error) {
	if d := os.Getenv("JIN_HOME"); d != "" {
		return d, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".jin"), nil
}

func loadOrCreateToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 32 {
			return t, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

func defaultOperator() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "operator"
}

func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "127.0.0.1" || host == "::1" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

// openBrowser launches the platform URL handler; url is built locally from the listen address and token.
func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url) //nolint:gosec // locally built URL
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url) //nolint:gosec // locally built URL
	default:
		c = exec.Command("xdg-open", url) //nolint:gosec // locally built URL
	}
	_ = c.Start()
}
