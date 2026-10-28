// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/httpapi"
	"agentplatform.local/platform/internal/identity"
	"agentplatform.local/platform/internal/jsonwire"
	"agentplatform.local/platform/internal/storage"
	"agentplatform.local/platform/internal/worker"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"
)

func main() {
	if e := run(); e != nil {
		slog.Error("platform stopped", "error", e.Error())
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: platform version|bundle|capabilities|doctor|config|migrate|bootstrap|serve|worker|mcp-stdio|link-identity|issue-token|issue-service-token|recover-login-policy|advance-event-replay|prepare-model|install-model|install-tool")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if os.Args[1] == "version" {
		return runVersion(os.Args[2:], os.Stdout)
	}
	if os.Args[1] == "bundle" {
		return runBundle(os.Args[2:], os.Stdout)
	}
	if os.Args[1] == "generate-webhook-key" {
		return runGenerateWebhookKey(os.Args[2:], os.Stdout)
	}
	if os.Args[1] == "prepare-model" {
		return runPrepareModel(os.Args[2:], os.Stdout)
	}
	if os.Args[1] == "config" {
		return runConfigurationClient(ctx, os.Args[2:], os.Stdout)
	}
	if os.Args[1] == "doctor" {
		return runDoctorClient(ctx, os.Args[2:], os.Stdout)
	}
	if os.Args[1] == "capabilities" {
		return runConfigurationClient(ctx, append([]string{"capabilities"}, os.Args[2:]...), os.Stdout)
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	s, e := storage.Open(ctx, dsn)
	if e != nil {
		return errors.New("cannot connect to PostgreSQL; check local database configuration")
	}
	defer s.Close()
	if os.Args[1] != "migrate" {
		if e = s.CheckRuntimeAdmission(ctx); e != nil {
			return e
		}
	}
	switch os.Args[1] {
	case "install-webhook-destination", "retire-webhook-destination":
		return runWebhookHost(ctx, s, os.Args[1], os.Args[2:], os.Stdout)
	case "install-credential-purpose", "install-configuration-destination":
		f := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
		path := f.String("file", "", "private approved configuration profile JSON file")
		if e = f.Parse(os.Args[2:]); e != nil {
			return e
		}
		if f.NArg() != 0 {
			return errors.New("unexpected command arguments")
		}
		if os.Args[1] == "install-credential-purpose" {
			var in domain.CredentialPurposeProfile
			if readPrivateJSON(*path, &in) != nil {
				return errors.New("invalid private credential-purpose profile")
			}
			if _, e = s.InstallCredentialPurposeProfile(ctx, in); e != nil {
				return errors.New("credential-purpose installation rejected; inspect trusted configuration and current version")
			}
		} else {
			var in domain.ConfigurationDestinationProfile
			if readPrivateJSON(*path, &in) != nil {
				return errors.New("invalid private destination profile")
			}
			if _, e = s.InstallConfigurationDestination(ctx, in); e != nil {
				return errors.New("destination installation rejected; inspect exact purpose pairing and version")
			}
		}
		fmt.Println("Approved configuration profile installed")
		return nil
	case "advance-event-replay":
		return runAdvanceEventReplay(ctx, s, os.Args[2:])
	case "issue-token":
		return runIssueMemberToken(ctx, s, os.Args[2:])
	case "issue-service-token":
		return runIssueServiceToken(ctx, s, os.Args[2:])
	case "recover-login-policy":
		return runRecoverLoginPolicy(ctx, s, os.Args[2:])
	case "mcp-stdio":
		return runMCPStdio(ctx, s, os.Args[2:])
	case "migrate":
		if e = s.Migrate(ctx); e != nil {
			return e
		}
		fmt.Println("Migrations applied")
		return nil
	case "bootstrap":
		f := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
		name := f.String("name", "Local organization", "organization name")
		budget := f.Int64("budget-microusd", 100_000_000, "organization budget")
		out := f.String("out", ".local/bootstrap.json", "new private credentials file")
		if e = f.Parse(os.Args[2:]); e != nil {
			return e
		}
		if e = os.MkdirAll(filepath.Dir(*out), 0700); e != nil {
			return e
		}
		file, e := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return errors.New("bootstrap output must be a new private file")
		}
		defer file.Close()
		b, e := s.Bootstrap(ctx, *name, *budget)
		if e != nil {
			_ = os.Remove(*out)
			return e
		}
		if e = json.NewEncoder(file).Encode(b); e != nil {
			return e
		}
		if e = file.Sync(); e != nil {
			return e
		}
		fmt.Println("Organization created; private credentials:", *out)
		return nil
	case "worker":
		f := flag.NewFlagSet("worker", flag.ContinueOnError)
		concurrency := f.Int("concurrency", 4, "concurrent worker slots (1..64)")
		if e = f.Parse(os.Args[2:]); e != nil {
			return e
		}
		if f.NArg() != 0 || *concurrency < 1 || *concurrency > 64 {
			return errors.New("worker concurrency must be 1..64")
		}
		w := worker.Worker{Store: s, Log: slog.Default(), Concurrency: *concurrency}
		return w.Run(ctx)
	case "link-identity":
		f := flag.NewFlagSet("link-identity", flag.ContinueOnError)
		issuer := f.String("issuer", "", "trusted HTTPS issuer")
		subject := f.String("subject", "", "exact stable subject")
		principal := f.String("principal", "", "existing principal ID")
		if e = f.Parse(os.Args[2:]); e != nil {
			return e
		}
		if f.NArg() != 0 {
			return errors.New("unexpected command arguments")
		}
		if e = s.LinkIdentity(ctx, *issuer, *subject, *principal); e != nil {
			return errors.New("identity link rejected; verify existing principal and trusted mapping")
		}
		fmt.Println("Identity linked to the existing principal")
		return nil
	case "install-tool":
		f := flag.NewFlagSet("install-tool", flag.ContinueOnError)
		path := f.String("file", "", "private tool connection JSON file")
		if e = f.Parse(os.Args[2:]); e != nil {
			return e
		}
		if f.NArg() != 0 {
			return errors.New("unexpected command arguments")
		}
		var connection domain.ToolConnection
		if readPrivateJSON(*path, &connection) != nil {
			return errors.New("invalid private tool configuration")
		}
		installed, err := s.InstallToolConnection(ctx, connection)
		if err != nil {
			return errors.New("tool installation rejected; verify immutable configuration")
		}
		fmt.Printf("Tool connection installed: %s version %d\n", installed.Tool.ID, installed.Tool.Version)
		return nil
	case "install-model":
		f := flag.NewFlagSet("install-model", flag.ContinueOnError)
		path := f.String("file", "", "private model connection JSON file")
		if e = f.Parse(os.Args[2:]); e != nil {
			return e
		}
		if f.NArg() != 0 {
			return errors.New("unexpected command arguments")
		}
		var model domain.ModelConnection
		if e = readPrivateJSON(*path, &model); e != nil {
			return errors.New("invalid private model configuration")
		}
		installed, e := s.InstallModelConnection(ctx, model)
		if e != nil {
			return errors.New("model installation rejected; verify configuration and immutable version")
		}
		fmt.Printf("Model connection installed: %s version %d\n", installed.ID, installed.Version)
		return nil
	case "serve":
		f := flag.NewFlagSet("serve", flag.ContinueOnError)
		addr := f.String("listen", "127.0.0.1:8080", "loopback listen address")
		containerPreview := f.Bool("container-preview", false, "explicit test-data container listener at 0.0.0.0:8080; publish only on host loopback")
		static := f.String("web", "", "explicit web assets; native packages default to their verified installed assets")
		oidcFile := f.String("oidc-config", "", "private OIDC configuration JSON file")
		tlsCert := f.String("tls-cert", "", "TLS certificate PEM file")
		tlsKey := f.String("tls-key", "", "private TLS key PEM file")
		tlsProxy := f.Bool("trusted-tls-proxy", false, "HTTPS is terminated by a trusted proxy to this loopback listener")
		mcpEnabled := f.Bool("mcp", false, "enable the private platform-token MCP adapter; this is not an OAuth resource server")
		mcpMutations := f.String("mcp-mutations", "", "explicit comma-separated MCP mutation tool names; default read-only")
		mcpOrigins := f.String("mcp-origins", "", "exact comma-separated allowed MCP browser origins")
		if e = f.Parse(os.Args[2:]); e != nil {
			return e
		}
		if e = validateServeListener(*addr, *containerPreview, *oidcFile != "", *mcpEnabled, *tlsProxy); e != nil {
			return e
		}
		if *static == "" {
			*static, e = nativeDefaultWeb()
			if e != nil {
				return e
			}
		}
		api := httpapi.Server{Store: s, Log: slog.Default(), StaticDir: *static}
		if f.NArg() != 0 || (*tlsCert == "") != (*tlsKey == "") {
			return errors.New("invalid serve arguments")
		}
		if *mcpEnabled && *tlsCert == "" && !*tlsProxy {
			return errors.New("HTTP MCP requires TLS or an explicitly trusted TLS-terminating proxy")
		}
		if *oidcFile != "" {
			if *tlsCert == "" && !*tlsProxy {
				return errors.New("OIDC requires TLS or an explicitly trusted TLS-terminating proxy")
			}
			cfg, encryptionKey, err := loadOIDCConfig(*oidcFile)
			if err != nil {
				return err
			}
			if err = s.ConfigureLoginEncryption(encryptionKey); err != nil {
				return errors.New("cannot initialize private login storage")
			}
			login, err := identity.NewLoginService(ctx, cfg, s)
			if err != nil {
				return errors.New("cannot initialize OIDC; verify administrator configuration and provider availability")
			}
			defer login.Close()
			api.Login = login
		}
		if *tlsKey != "" {
			if _, err := readPrivateFile(*tlsKey, 64<<10); err != nil {
				return errors.New("TLS key must be a private regular file")
			}
		}
		handler, e := mcpHTTPHandler(api.Handler(), *mcpEnabled, *mcpMutations, *mcpOrigins)
		if e != nil {
			return e
		}
		srv := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
		done := make(chan error, 1)
		go func() {
			slog.Info("platform listener started", "address", *addr, "oidc_enabled", api.Login != nil, "tls", *tlsCert != "", "private_mcp", *mcpEnabled)
			if *tlsCert != "" {
				done <- srv.ListenAndServeTLS(*tlsCert, *tlsKey)
			} else {
				done <- srv.ListenAndServe()
			}
		}()
		select {
		case e = <-done:
			if errors.Is(e, http.ErrServerClosed) {
				return nil
			}
			return e
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return srv.Shutdown(shutdown)
		}
	default:
		return errors.New("unknown command")
	}
}

type secretReference struct {
	Environment string `json:"environment"`
	File        string `json:"file"`
}
type oidcFileConfig struct {
	Issuer                string          `json:"issuer"`
	ClientID              string          `json:"client_id"`
	PublicOrigin          string          `json:"public_origin"`
	RedirectURI           string          `json:"redirect_uri"`
	TokenAuthMethod       string          `json:"token_auth_method"`
	ClientSecret          secretReference `json:"client_secret_ref"`
	DeploymentSeed        secretReference `json:"deployment_seed_ref"`
	RootCAFile            string          `json:"root_ca_file"`
	Algorithms            []string        `json:"allowed_signing_algorithms"`
	EndpointOrigins       []string        `json:"additional_endpoint_origins"`
	JWKSOrigins           []string        `json:"additional_jwks_origins"`
	RequestTimeoutMS      int64           `json:"request_timeout_ms"`
	TransactionTTLSeconds int64           `json:"transaction_ttl_seconds"`
	SessionTTLSeconds     int64           `json:"session_ttl_seconds"`
}

var oidcSecretEnvironment = regexp.MustCompile(`^AGENT_PLATFORM_OIDC_[A-Z0-9_]+$`)

func loadOIDCConfig(path string) (identity.LoginConfig, []byte, error) {
	invalid := errors.New("invalid private OIDC configuration or secret reference")
	var file oidcFileConfig
	if readPrivateJSON(path, &file) != nil {
		return identity.LoginConfig{}, nil, invalid
	}
	dir := filepath.Dir(path)
	seedText, e := readOIDCSecret(file.DeploymentSeed, dir)
	if e != nil {
		return identity.LoginConfig{}, nil, invalid
	}
	seed, e := base64.StdEncoding.Strict().DecodeString(seedText)
	if e != nil || len(seed) < 32 || len(seed) > 64 {
		return identity.LoginConfig{}, nil, invalid
	}
	derive := func(label string) []byte {
		h := hmac.New(sha256.New, seed)
		_, _ = h.Write([]byte(label))
		return h.Sum(nil)
	}
	if file.RequestTimeoutMS < 0 || file.RequestTimeoutMS > 30000 || file.TransactionTTLSeconds < 0 || file.TransactionTTLSeconds > 600 || file.SessionTTLSeconds < 0 || file.SessionTTLSeconds > 86400 {
		return identity.LoginConfig{}, nil, invalid
	}
	cfg := identity.LoginConfig{Provider: identity.Config{IssuerURL: file.Issuer, ClientID: file.ClientID, AllowedSigningAlgorithms: file.Algorithms, AdditionalJWKSOrigins: file.JWKSOrigins, RequestTimeout: time.Duration(file.RequestTimeoutMS) * time.Millisecond}, PublicOrigin: file.PublicOrigin, RedirectURI: file.RedirectURI, TokenAuthMethod: file.TokenAuthMethod, AdditionalEndpointOrigins: file.EndpointOrigins, CSRFKey: derive("agent-platform-oidc/csrf/v1"), TransactionTTL: time.Duration(file.TransactionTTLSeconds) * time.Second, SessionTTL: time.Duration(file.SessionTTLSeconds) * time.Second}
	if file.TokenAuthMethod == "client_secret_basic" {
		cfg.ClientSecret, e = readOIDCSecret(file.ClientSecret, dir)
		if e != nil {
			return identity.LoginConfig{}, nil, invalid
		}
	} else if file.ClientSecret.Environment != "" || file.ClientSecret.File != "" {
		return identity.LoginConfig{}, nil, invalid
	}
	if file.RootCAFile != "" {
		p := file.RootCAFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		pem, e := readPrivateFile(p, 1<<20)
		if e != nil {
			return identity.LoginConfig{}, nil, invalid
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return identity.LoginConfig{}, nil, invalid
		}
		cfg.Provider.RootCAs = pool
	}
	return cfg, derive("agent-platform-oidc/pkce-storage/v1"), nil
}

func readOIDCSecret(ref secretReference, dir string) (string, error) {
	invalid := errors.New("invalid secret reference")
	if (ref.Environment == "") == (ref.File == "") {
		return "", invalid
	}
	var value string
	if ref.Environment != "" {
		if !oidcSecretEnvironment.MatchString(ref.Environment) {
			return "", invalid
		}
		value = os.Getenv(ref.Environment)
	} else {
		path := ref.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		data, e := readPrivateFile(path, 16<<10)
		if e != nil {
			return "", invalid
		}
		value = strings.TrimRight(string(data), "\r\n")
	}
	if value == "" || len(value) > 16384 || strings.ContainsRune(value, '\x00') {
		return "", invalid
	}
	return value, nil
}

func readPrivateFile(path string, max int64) ([]byte, error) {
	invalid := errors.New("private regular file required")
	before, e := os.Lstat(path)
	if e != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 {
		return nil, invalid
	}
	if stat, ok := before.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, invalid
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, invalid
	}
	defer f.Close()
	after, e := f.Stat()
	if e != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Size() > max {
		return nil, invalid
	}
	data, e := io.ReadAll(io.LimitReader(f, max+1))
	if e != nil || int64(len(data)) > max {
		return nil, invalid
	}
	return data, nil
}

func readPrivateJSON(path string, out any) error {
	data, e := readPrivateFile(path, 1<<20)
	if e != nil {
		return e
	}
	if !jsonwire.ValidUnicode(data) {
		return errors.New("invalid Unicode in private configuration")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if e = strictConfigValue(d, reflect.TypeOf(out).Elem(), 0); e != nil {
		return e
	}
	if _, e = d.Token(); e != io.EOF {
		return errors.New("one JSON object required")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func strictConfigValue(d *json.Decoder, typ reflect.Type, depth int) error {
	invalid := errors.New("invalid configuration JSON")
	if depth > 32 {
		return invalid
	}
	tok, e := d.Token()
	if e != nil || tok == nil {
		return invalid
	}
	if str, ok := tok.(string); ok && strings.ContainsRune(str, '\x00') {
		return invalid
	}
	// Omitted optional fields stay nil during decoding. A present pointer has
	// the same JSON shape as its element; explicit null remains rejected above.
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		if typ.Kind() != reflect.Struct {
			return invalid
		}
		allowed := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			allowed[strings.Split(f.Tag.Get("json"), ",")[0]] = f.Type
		}
		seen := map[string]bool{}
		for d.More() {
			key, e := d.Token()
			name, ok := key.(string)
			ft, present := allowed[name]
			if e != nil || !ok || !present || seen[name] {
				return invalid
			}
			seen[name] = true
			if e = strictConfigValue(d, ft, depth+1); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim('}') {
			return invalid
		}
	case '[':
		if typ.Kind() != reflect.Slice {
			return invalid
		}
		for d.More() {
			if e = strictConfigValue(d, typ.Elem(), depth+1); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim(']') {
			return invalid
		}
	default:
		return invalid
	}
	return nil
}
