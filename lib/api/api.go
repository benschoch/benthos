package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/pprof"
	"runtime"
	"sync"
	"time"

	"github.com/Jeffail/benthos/v3/lib/log"
	"github.com/Jeffail/benthos/v3/lib/metrics"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	yaml "gopkg.in/yaml.v3"
)

//------------------------------------------------------------------------------

// Config contains the configuration fields for the Benthos API.
type Config struct {
	Address        string `json:"address" yaml:"address"`
	Enabled        bool   `json:"enabled" yaml:"enabled"`
	ReadTimeout    string `json:"read_timeout" yaml:"read_timeout"`
	RootPath       string `json:"root_path" yaml:"root_path"`
	DebugEndpoints bool   `json:"debug_endpoints" yaml:"debug_endpoints"`
	CertFile       string `json:"cert_file" yaml:"cert_file"`
	KeyFile        string `json:"key_file" yaml:"key_file"`
	EnableCORS     bool   `json:"enable_cors" yaml:"enable_cors"`
}

// NewConfig creates a new API config with default values.
func NewConfig() Config {
	return Config{
		Address:        "0.0.0.0:4195",
		Enabled:        true,
		ReadTimeout:    "5s",
		RootPath:       "/benthos",
		DebugEndpoints: false,
		CertFile:       "",
		KeyFile:        "",
		EnableCORS:     false,
	}
}

//------------------------------------------------------------------------------

// OptFunc applies an option to an API type during construction.
type OptFunc func(t *Type)

// OptWithMiddleware adds an HTTP middleware to the Benthos API.
func OptWithMiddleware(m func(http.Handler) http.Handler) OptFunc {
	return func(t *Type) {
		t.server.Handler = m(t.server.Handler)
	}
}

// OptWithTLS replaces the tls options of the HTTP server.
func OptWithTLS(tls *tls.Config) OptFunc {
	return func(t *Type) {
		t.server.TLSConfig = tls
	}
}

//------------------------------------------------------------------------------

// Type implements the Benthos HTTP API.
type Type struct {
	conf         Config
	endpoints    map[string]string
	endpointsMut sync.Mutex

	ctx    context.Context
	cancel func()

	handlers    map[string]http.HandlerFunc
	handlersMut sync.RWMutex

	log    log.Modular
	mux    *mux.Router
	server *http.Server
}

// New creates a new Benthos HTTP API.
func New(
	version string,
	dateBuilt string,
	conf Config,
	wholeConf interface{},
	log log.Modular,
	stats metrics.Type,
	opts ...OptFunc,
) (*Type, error) {
	gMux := mux.NewRouter()

	var handler http.Handler = gMux
	if conf.EnableCORS {
		handler = handlers.CORS(
			handlers.AllowedMethods([]string{"GET", "HEAD", "POST", "DELETE"}),
		)(gMux)
	}

	server := &http.Server{
		Addr:    conf.Address,
		Handler: handler,
	}

	if conf.CertFile != "" || conf.KeyFile != "" {
		if conf.CertFile == "" || conf.KeyFile == "" {
			return nil, errors.New("both cert_file and key_file must be specified, or neither")
		}
	}

	if tout := conf.ReadTimeout; len(tout) > 0 {
		var err error
		if server.ReadTimeout, err = time.ParseDuration(tout); err != nil {
			return nil, fmt.Errorf("failed to parse read timeout string: %v", err)
		}
	}
	t := &Type{
		conf:      conf,
		endpoints: map[string]string{},
		handlers:  map[string]http.HandlerFunc{},
		mux:       gMux,
		server:    server,
		log:       log,
	}
	t.ctx, t.cancel = context.WithCancel(context.Background())

	handlePing := func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("pong"))
	}

	handleStackTrace := func(w http.ResponseWriter, r *http.Request) {
		stackSlice := make([]byte, 1024*100)
		s := runtime.Stack(stackSlice, true)
		w.Write(stackSlice[:s])
	}

	handlePrintJSONConfig := func(w http.ResponseWriter, r *http.Request) {
		var g interface{}
		var err error
		if node, ok := wholeConf.(yaml.Node); ok {
			err = node.Decode(&g)
		} else {
			g = node
		}
		var resBytes []byte
		if err == nil {
			resBytes, err = json.Marshal(g)
		}
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write(resBytes)
	}

	handlePrintYAMLConfig := func(w http.ResponseWriter, r *http.Request) {
		resBytes, err := yaml.Marshal(wholeConf)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write(resBytes)
	}

	handleVersion := func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(fmt.Sprintf("{\"version\":\"%v\", \"built\":\"%v\"}", version, dateBuilt)))
	}

	handleEndpoints := func(w http.ResponseWriter, r *http.Request) {
		t.endpointsMut.Lock()
		defer t.endpointsMut.Unlock()

		resBytes, err := json.Marshal(t.endpoints)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
		} else {
			w.Write(resBytes)
		}
	}

	if t.conf.DebugEndpoints {
		t.RegisterEndpoint(
			"/debug/config/json", "DEBUG: Returns the loaded config as JSON.",
			handlePrintJSONConfig,
		)
		t.RegisterEndpoint(
			"/debug/config/yaml", "DEBUG: Returns the loaded config as YAML.",
			handlePrintYAMLConfig,
		)
		t.RegisterEndpoint(
			"/debug/stack", "DEBUG: Returns a snapshot of the current service stack trace.",
			handleStackTrace,
		)
		t.RegisterEndpoint(
			"/debug/pprof/profile", "DEBUG: Responds with a pprof-formatted cpu profile.",
			pprof.Profile,
		)
		t.RegisterEndpoint(
			"/debug/pprof/heap", "DEBUG: Responds with a pprof-formatted heap profile.",
			pprof.Index,
		)
		t.RegisterEndpoint(
			"/debug/pprof/block", "DEBUG: Responds with a pprof-formatted block profile.",
			pprof.Index,
		)
		t.RegisterEndpoint(
			"/debug/pprof/mutex", "DEBUG: Responds with a pprof-formatted mutex profile.",
			pprof.Index,
		)
		t.RegisterEndpoint(
			"/debug/pprof/symbol", "DEBUG: looks up the program counters listed"+
				" in the request, responding with a table mapping program"+
				" counters to function names.",
			pprof.Symbol,
		)
		t.RegisterEndpoint(
			"/debug/pprof/trace",
			"DEBUG: Responds with the execution trace in binary form."+
				" Tracing lasts for duration specified in seconds GET"+
				" parameter, or for 1 second if not specified.",
			pprof.Trace,
		)
	}

	t.RegisterEndpoint("/ping", "Ping me.", handlePing)
	t.RegisterEndpoint("/version", "Returns the service version.", handleVersion)
	t.RegisterEndpoint("/endpoints", "Returns this map of endpoints.", handleEndpoints)

	// If we want to expose a JSON stats endpoint we register the endpoints.
	if wHandlerFunc, ok := stats.(metrics.WithHandlerFunc); ok {
		t.RegisterEndpoint(
			"/stats", "Returns a JSON object of service metrics.",
			wHandlerFunc.HandlerFunc(),
		)
		t.RegisterEndpoint(
			"/metrics", "Returns a JSON object of service metrics.",
			wHandlerFunc.HandlerFunc(),
		)
	}

	for _, opt := range opts {
		opt(t)
	}

	return t, nil
}

// RegisterEndpoint registers a http.HandlerFunc under a path with a
// description that will be displayed under the /endpoints path.
func (t *Type) RegisterEndpoint(path, desc string, handlerFunc http.HandlerFunc) {
	t.endpointsMut.Lock()
	defer t.endpointsMut.Unlock()

	t.endpoints[path] = desc

	t.handlersMut.Lock()
	defer t.handlersMut.Unlock()

	if _, exists := t.handlers[path]; !exists {
		wrapHandler := func(w http.ResponseWriter, r *http.Request) {
			t.handlersMut.RLock()
			h := t.handlers[path]
			t.handlersMut.RUnlock()
			h(w, r)
		}
		t.mux.HandleFunc(path, wrapHandler)
		t.mux.HandleFunc(t.conf.RootPath+path, wrapHandler)
	}
	t.handlers[path] = handlerFunc
}

// ListenAndServe launches the API and blocks until the server closes or fails.
func (t *Type) ListenAndServe() error {
	if !t.conf.Enabled {
		<-t.ctx.Done()
		return nil
	}
	t.log.Infof(
		"Listening for HTTP requests at: %v\n",
		"http://"+t.conf.Address,
	)
	if t.server.TLSConfig != nil {
		return t.server.ListenAndServeTLS("", "")
	}
	if len(t.conf.CertFile) > 0 {
		return t.server.ListenAndServeTLS(t.conf.CertFile, t.conf.KeyFile)
	}
	return t.server.ListenAndServe()
}

// Shutdown attempts to close the http server.
func (t *Type) Shutdown(ctx context.Context) error {
	t.cancel()
	return t.server.Shutdown(ctx)
}

//------------------------------------------------------------------------------
