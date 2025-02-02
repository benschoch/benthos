package writer

import (
	"context"
	"fmt"
	"time"

	"github.com/benthosdev/benthos/v4/internal/bloblang/field"
	"github.com/benthosdev/benthos/v4/internal/component/cache"
	"github.com/benthosdev/benthos/v4/internal/component/metrics"
	"github.com/benthosdev/benthos/v4/internal/interop"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/message"
)

//------------------------------------------------------------------------------

// CacheConfig contains configuration fields for the Cache output type.
type CacheConfig struct {
	Target      string `json:"target" yaml:"target"`
	Key         string `json:"key" yaml:"key"`
	TTL         string `json:"ttl" yaml:"ttl"`
	MaxInFlight int    `json:"max_in_flight" yaml:"max_in_flight"`
}

// NewCacheConfig creates a new Config with default values.
func NewCacheConfig() CacheConfig {
	return CacheConfig{
		Target:      "",
		Key:         `${!count("items")}-${!timestamp_unix_nano()}`,
		MaxInFlight: 64,
	}
}

//------------------------------------------------------------------------------

// Cache is a benthos writer.Type implementation that writes messages to a
// Cache directory.
type Cache struct {
	conf CacheConfig
	mgr  interop.Manager

	key *field.Expression
	ttl *field.Expression

	log   log.Modular
	stats metrics.Type
}

// NewCache creates a new Cache writer.Type.
func NewCache(
	conf CacheConfig,
	mgr interop.Manager,
	log log.Modular,
	stats metrics.Type,
) (*Cache, error) {
	key, err := mgr.BloblEnvironment().NewField(conf.Key)
	if err != nil {
		return nil, fmt.Errorf("failed to parse key expression: %v", err)
	}
	ttl, err := mgr.BloblEnvironment().NewField(conf.TTL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ttl expression: %v", err)
	}
	if !mgr.ProbeCache(conf.Target) {
		return nil, fmt.Errorf("cache resource '%v' was not found", conf.Target)
	}
	return &Cache{
		conf:  conf,
		mgr:   mgr,
		key:   key,
		ttl:   ttl,
		log:   log,
		stats: stats,
	}, nil
}

// ConnectWithContext does nothing.
func (c *Cache) ConnectWithContext(ctx context.Context) error {
	return c.Connect()
}

// Connect does nothing.
func (c *Cache) Connect() error {
	c.log.Infof("Writing message parts as items in cache: %v\n", c.conf.Target)
	return nil
}

func (c *Cache) writeMulti(ctx context.Context, msg *message.Batch) error {
	var err error
	if cerr := c.mgr.AccessCache(ctx, c.conf.Target, func(ac cache.V1) {
		items := map[string]cache.TTLItem{}
		if err = msg.Iter(func(i int, p *message.Part) error {
			var ttl *time.Duration
			if ttls := c.ttl.String(i, msg); ttls != "" {
				t, terr := time.ParseDuration(ttls)
				if terr != nil {
					c.log.Debugf("Invalid duration string for TTL field: %v\n", terr)
					return fmt.Errorf("ttl field: %w", terr)
				}
				ttl = &t
			}
			items[c.key.String(i, msg)] = cache.TTLItem{
				Value: p.Get(),
				TTL:   ttl,
			}
			return nil
		}); err != nil {
			return
		}
		err = ac.SetMulti(ctx, items)
	}); cerr != nil {
		err = cerr
	}
	return err
}

// WriteWithContext attempts to write message contents to a target Cache.
func (c *Cache) WriteWithContext(ctx context.Context, msg *message.Batch) error {
	if msg.Len() > 1 {
		return c.writeMulti(ctx, msg)
	}
	var err error
	if cerr := c.mgr.AccessCache(ctx, c.conf.Target, func(cache cache.V1) {
		var ttl *time.Duration
		if ttls := c.ttl.String(0, msg); ttls != "" {
			t, terr := time.ParseDuration(ttls)
			if terr != nil {
				c.log.Debugf("Invalid duration string for TTL field: %v\n", terr)
				err = fmt.Errorf("ttl field: %w", terr)
				return
			}
			ttl = &t
		}
		err = cache.Set(ctx, c.key.String(0, msg), msg.Get(0).Get(), ttl)
	}); cerr != nil {
		err = cerr
	}
	return err
}

// Write attempts to write message contents to a target Cache.
func (c *Cache) Write(msg *message.Batch) error {
	return c.WriteWithContext(context.Background(), msg)
}

// CloseAsync begins cleaning up resources used by this writer asynchronously.
func (c *Cache) CloseAsync() {
}

// WaitForClose will block until either the writer is closed or a specified
// timeout occurs.
func (c *Cache) WaitForClose(time.Duration) error {
	return nil
}

//------------------------------------------------------------------------------
