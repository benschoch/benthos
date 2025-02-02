package writer

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/benthosdev/benthos/v4/internal/component"
	"github.com/benthosdev/benthos/v4/internal/component/metrics"
	"github.com/benthosdev/benthos/v4/internal/http/docs/auth"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/message"
	btls "github.com/benthosdev/benthos/v4/internal/tls"
)

//------------------------------------------------------------------------------

// WebsocketConfig contains configuration fields for the Websocket output type.
type WebsocketConfig struct {
	URL         string `json:"url" yaml:"url"`
	auth.Config `json:",inline" yaml:",inline"`
	TLS         btls.Config `json:"tls" yaml:"tls"`
}

// NewWebsocketConfig creates a new WebsocketConfig with default values.
func NewWebsocketConfig() WebsocketConfig {
	return WebsocketConfig{
		URL:    "",
		Config: auth.NewConfig(),
		TLS:    btls.NewConfig(),
	}
}

//------------------------------------------------------------------------------

// Websocket is an output type that serves Websocket messages.
type Websocket struct {
	log   log.Modular
	stats metrics.Type

	lock *sync.Mutex

	conf    WebsocketConfig
	client  *websocket.Conn
	tlsConf *tls.Config
}

// NewWebsocket creates a new Websocket output type.
func NewWebsocket(
	conf WebsocketConfig,
	log log.Modular,
	stats metrics.Type,
) (*Websocket, error) {
	ws := &Websocket{
		log:   log,
		stats: stats,
		lock:  &sync.Mutex{},
		conf:  conf,
	}
	if conf.TLS.Enabled {
		var err error
		if ws.tlsConf, err = conf.TLS.Get(); err != nil {
			return nil, err
		}
	}
	return ws, nil
}

//------------------------------------------------------------------------------

func (w *Websocket) getWS() *websocket.Conn {
	w.lock.Lock()
	ws := w.client
	w.lock.Unlock()
	return ws
}

//------------------------------------------------------------------------------

// ConnectWithContext establishes a connection to an Websocket server.
func (w *Websocket) ConnectWithContext(ctx context.Context) error {
	w.lock.Lock()
	defer w.lock.Unlock()

	if w.client != nil {
		return nil
	}

	headers := http.Header{}

	purl, err := url.Parse(w.conf.URL)
	if err != nil {
		return err
	}

	if err := w.conf.Sign(&http.Request{
		URL:    purl,
		Header: headers,
	}); err != nil {
		return err
	}

	var client *websocket.Conn
	if w.conf.TLS.Enabled {
		dialer := websocket.Dialer{
			TLSClientConfig: w.tlsConf,
		}
		if client, _, err = dialer.Dial(w.conf.URL, headers); err != nil {
			return err

		}
	} else if client, _, err = websocket.DefaultDialer.Dial(w.conf.URL, headers); err != nil {
		return err
	}

	go func(c *websocket.Conn) {
		for {
			if _, _, cerr := c.NextReader(); cerr != nil {
				c.Close()
				break
			}
		}
	}(client)

	w.client = client
	return nil
}

//------------------------------------------------------------------------------

// WriteWithContext attempts to write a message by pushing it to an Websocket broker.
func (w *Websocket) WriteWithContext(ctx context.Context, msg *message.Batch) error {
	client := w.getWS()
	if client == nil {
		return component.ErrNotConnected
	}

	err := msg.Iter(func(i int, p *message.Part) error {
		return client.WriteMessage(websocket.BinaryMessage, p.Get())
	})
	if err != nil {
		w.lock.Lock()
		w.client = nil
		w.lock.Unlock()
		if err == websocket.ErrCloseSent {
			return component.ErrNotConnected
		}
		return err
	}
	return nil
}

// CloseAsync shuts down the Websocket output and stops processing messages.
func (w *Websocket) CloseAsync() {
	go func() {
		w.lock.Lock()
		if w.client != nil {
			w.client.Close()
			w.client = nil
		}
		w.lock.Unlock()
	}()
}

// WaitForClose blocks until the Websocket output has closed down.
func (w *Websocket) WaitForClose(timeout time.Duration) error {
	return nil
}

//------------------------------------------------------------------------------
