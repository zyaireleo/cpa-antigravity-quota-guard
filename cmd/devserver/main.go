package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/runtime"
)

type devServerOptions struct {
	AuthDir        string
	QuotaStatePath string
	StateCachePath string
	AccountCount   int
	Seed           int64
	Clock          runtime.Clock
}

type devServer struct {
	host    *devHost
	runtime *runtime.Runtime
}

func newDevServer(options devServerOptions) (*devServer, error) {
	if strings.TrimSpace(options.AuthDir) == "" {
		options.AuthDir = defaultDevAuthDir
	}
	if strings.TrimSpace(options.QuotaStatePath) == "" {
		options.QuotaStatePath = defaultDevQuotaState
	}
	if strings.TrimSpace(options.StateCachePath) == "" {
		options.StateCachePath = defaultDevCachePath
	}
	if options.AccountCount <= 0 {
		options.AccountCount = defaultDevAccountCount
	}

	var nowFn func() time.Time
	if options.Clock != nil {
		nowFn = options.Clock.Now
	}
	fakeHost, err := newDevHost(devHostOptions{
		AuthDir:        options.AuthDir,
		QuotaStatePath: options.QuotaStatePath,
		AccountCount:   options.AccountCount,
		Seed:           options.Seed,
		NowFn:          nowFn,
	})
	if err != nil {
		return nil, err
	}
	rt := runtime.New(runtime.Options{
		Host:           fakeHost,
		Clock:          options.Clock,
		StateCachePath: options.StateCachePath,
		GuardStatePath: filepath.Join(filepath.Dir(options.StateCachePath), "guard-state.json"),
	})
	if _, err := rt.Register(context.Background(), runtime.RegisterRequest{ConfigYAML: `{"enabled":true}`}); err != nil {
		_ = rt.Shutdown(context.Background())
		return nil, fmt.Errorf("register dev runtime: %w", err)
	}
	return &devServer{host: fakeHost, runtime: rt}, nil
}

func main() {
	defaultAddress := strings.TrimSpace(os.Getenv("ANTIGRAVITY_DEVSERVER_ADDR"))
	if defaultAddress == "" {
		defaultAddress = "127.0.0.1:8080"
	}
	defaultAccounts := envInt("ANTIGRAVITY_DEVSERVER_ACCOUNTS", defaultDevAccountCount)
	defaultSeed := envInt64("ANTIGRAVITY_DEVSERVER_SEED", 0)
	defaultAuthDir := envString("ANTIGRAVITY_DEVSERVER_AUTH_DIR", defaultDevAuthDir)
	defaultQuotaState := envString("ANTIGRAVITY_DEVSERVER_QUOTA_STATE", defaultDevQuotaState)
	defaultCachePath := envString("ANTIGRAVITY_DEVSERVER_CACHE", defaultDevCachePath)

	address := flag.String("addr", defaultAddress, "HTTP listen address")
	unsafeListen := flag.Bool("unsafe-listen", false, "allow a non-loopback listen address (unsafe)")
	managementKey := flag.String("management-key", envString("ANTIGRAVITY_DEVSERVER_MANAGEMENT_KEY", ""), "dev Management API key; generated when empty")
	authDir := flag.String("auth-dir", defaultAuthDir, "directory containing simulated CPA auth JSON files")
	quotaState := flag.String("quota-state", defaultQuotaState, "persistent state for simulated Antigravity quota")
	cachePath := flag.String("state-cache", defaultCachePath, "Runtime state cache path")
	accounts := flag.Int("accounts", defaultAccounts, "minimum number of simulated Antigravity auth files")
	seed := flag.Int64("seed", defaultSeed, "random seed for deterministic quota simulation; zero selects a time-based seed")
	flag.Parse()
	if err := validateDevListenAddress(*address, *unsafeListen); err != nil {
		log.Fatal(err)
	}
	if strings.TrimSpace(*managementKey) == "" {
		generated, err := generateDevManagementKey()
		if err != nil {
			log.Fatalf("generate dev management key: %v", err)
		}
		*managementKey = generated
	}

	dev, err := newDevServer(devServerOptions{
		AuthDir:        *authDir,
		QuotaStatePath: *quotaState,
		StateCachePath: *cachePath,
		AccountCount:   *accounts,
		Seed:           *seed,
	})
	if err != nil {
		log.Fatalf("start devserver runtime: %v", err)
	}

	server := &http.Server{
		Addr:              *address,
		Handler:           guardDevHandler(dev.runtime, *managementKey),
		ReadHeaderTimeout: 5 * time.Second,
	}
	stopContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Println("=========================================================")
	fmt.Println(" CPA Antigravity Quota Guard - Dev Server")
	fmt.Println("=========================================================")
	displayAddress := *address
	if strings.HasPrefix(displayAddress, ":") {
		displayAddress = "localhost" + displayAddress
	}
	fmt.Printf(" Status page: http://%s/v0/resource/plugins/cpa-antigravity-quota-guard/status\n", displayAddress)
	fmt.Printf(" Dev Management Key: %s\n", *managementKey)
	fmt.Printf(" Simulated CPA auth files: %s (%d minimum accounts)\n", filepath.Clean(*authDir), *accounts)
	fmt.Println(" Simulated Antigravity quota requests stay inside the devserver.")
	fmt.Println(" Press Ctrl+C to stop the server.")
	fmt.Println("=========================================================")

	serverError := make(chan error, 1)
	go func() {
		serverError <- server.ListenAndServe()
	}()
	select {
	case err := <-serverError:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("devserver error: %v", err)
		}
	case <-stopContext.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			log.Printf("devserver HTTP shutdown: %v", err)
		}
		if err := dev.runtime.Shutdown(shutdownContext); err != nil {
			log.Printf("devserver runtime shutdown: %v", err)
		}
		<-serverError
	}
}

func guardDevHandler(rt *runtime.Runtime, managementKey string) http.Handler {
	managementKey = strings.TrimSpace(managementKey)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/v0/management/") && !validDevManagementKey(request, managementKey) {
			writer.Header().Set("WWW-Authenticate", `Bearer realm="cpa-antigravity-quota-guard-dev"`)
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		if err != nil {
			http.Error(writer, "read request", http.StatusBadRequest)
			return
		}
		wire, err := json.Marshal(runtime.ManagementRequest{
			Method:  request.Method,
			Path:    request.URL.Path,
			Headers: request.Header.Clone(),
			Query:   request.URL.Query(),
			Body:    body,
		})
		if err != nil {
			http.Error(writer, "encode request", http.StatusInternalServerError)
			return
		}
		var envelope runtime.Envelope
		if err := json.Unmarshal(rt.Handle(request.Context(), runtime.MethodManagementHandle, wire), &envelope); err != nil || !envelope.OK {
			http.Error(writer, "management handler failure", http.StatusInternalServerError)
			return
		}
		var response runtime.ManagementResponse
		if err := json.Unmarshal(envelope.Result, &response); err != nil {
			http.Error(writer, "decode response", http.StatusInternalServerError)
			return
		}
		for name, values := range response.Headers {
			for _, value := range values {
				writer.Header().Add(name, value)
			}
		}
		decoded, err := base64.StdEncoding.DecodeString(response.Body)
		if err != nil {
			http.Error(writer, "decode response body", http.StatusInternalServerError)
			return
		}
		writer.WriteHeader(response.StatusCode)
		_, _ = writer.Write(decoded)
	})
}

func validateDevListenAddress(address string, unsafe bool) error {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return fmt.Errorf("invalid devserver listen address %q: %w", address, err)
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	loopback := strings.EqualFold(host, "localhost")
	if parsed := net.ParseIP(host); parsed != nil && parsed.IsLoopback() {
		loopback = true
	}
	if !loopback && !unsafe {
		return fmt.Errorf("refusing non-loopback devserver address %q without -unsafe-listen", address)
	}
	return nil
}

func generateDevManagementKey() (string, error) {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func validDevManagementKey(request *http.Request, expected string) bool {
	if request == nil || expected == "" {
		return false
	}
	provided := strings.TrimSpace(request.Header.Get("X-Management-Key"))
	if provided == "" {
		provided = strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
	}
	return len(provided) == len(expected) && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func envString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt64(key string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
