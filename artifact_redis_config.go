package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"io"
	"os"

	redisclient "github.com/soulteary/redis-kit/client"
)

const maxArtifactRedisCABytes = 1 << 20

func registerRedisArtifactFlags(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.ArtifactRedisAddr, "artifactRedisAddr", "localhost:6379", "Redis artifact cache host:port")
	fs.StringVar(&c.ArtifactRedisUsername, "artifactRedisUsername", "", "Redis ACL username")
	fs.StringVar(&c.ArtifactRedisPassword, "artifactRedisPassword", "", "Redis password; prefer the corresponding environment variable")
	fs.IntVar(&c.ArtifactRedisDB, "artifactRedisDB", 0, "nonnegative Redis database number")
	fs.StringVar(&c.ArtifactRedisPrefix, "artifactRedisPrefix", "favicon:artifacts:v1:", "Redis artifact namespace, at most 64 ASCII letters/digits/colon/underscore/hyphen")
	fs.DurationVar(&c.ArtifactRedisTimeout, "artifactRedisTimeout", defaultRedisArtifactConfig().OperationTimeout, "total Redis lookup/write budget, positive and at most 5s")
	fs.BoolVar(&c.ArtifactRedisTLS, "artifactRedisTLS", false, "use certificate-verified TLS for Redis")
	fs.StringVar(&c.ArtifactRedisCAFile, "artifactRedisCAFile", "", "optional PEM CA bundle for Redis TLS, at most 1 MiB")
	fs.StringVar(&c.ArtifactRedisServerName, "artifactRedisServerName", "", "optional verified Redis TLS server name")
}

func (c Config) redisArtifactConfig() redisArtifactConfig {
	cfg := defaultRedisArtifactConfig()
	cfg.Client = redisclient.DefaultConfig().WithPoolSize(8).WithMinIdleConns(0).
		WithAddr(c.ArtifactRedisAddr).WithUsername(c.ArtifactRedisUsername).
		WithPassword(c.ArtifactRedisPassword).WithDB(c.ArtifactRedisDB)
	cfg.Prefix, cfg.OperationTimeout = c.ArtifactRedisPrefix, c.ArtifactRedisTimeout
	if c.ArtifactRedisTLS {
		cfg.Client = cfg.Client.WithTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ArtifactRedisServerName})
	}
	return cfg
}

func validateRedisArtifactConfig(c *Config) error {
	if (!c.ArtifactRedisTLS && (c.ArtifactRedisCAFile != "" || c.ArtifactRedisServerName != "")) || (c.ArtifactRedisServerName != "" && !validConfigHost(c.ArtifactRedisServerName)) {
		return invalidConfig("artifactRedisTLS", "CA and server name require verified TLS with a valid host name")
	}
	if err := c.redisArtifactConfig().validate(1); err != nil {
		return invalidConfig("artifactRedis", "invalid address, database, namespace or operation budget")
	}
	return nil
}

func (c Config) newRedisArtifactBackend() (artifactStore, error) {
	cfg := c.redisArtifactConfig()
	if c.ArtifactRedisTLS && c.ArtifactRedisCAFile != "" {
		metadata, err := os.Stat(c.ArtifactRedisCAFile)
		if err != nil || !metadata.Mode().IsRegular() || metadata.Size() > maxArtifactRedisCABytes {
			return nil, invalidConfig("artifactRedisCAFile", "CA bundle must be a regular file of at most 1 MiB")
		}
		file, err := os.Open(c.ArtifactRedisCAFile)
		if err != nil {
			return nil, invalidConfig("artifactRedisCAFile", "cannot read CA bundle")
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() > maxArtifactRedisCABytes {
			_ = file.Close()
			return nil, invalidConfig("artifactRedisCAFile", "CA bundle must be a regular file of at most 1 MiB")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxArtifactRedisCABytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(data) > maxArtifactRedisCABytes {
			return nil, invalidConfig("artifactRedisCAFile", "cannot read bounded CA bundle")
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return nil, invalidConfig("artifactRedisCAFile", "invalid PEM CA bundle")
		}
		cfg.Client.TLSConfig.RootCAs = roots
	}
	store, err := newRecoveringRedisArtifactStore(cfg, c.artifactEntryLimit())
	if err != nil {
		return nil, invalidConfig("artifactRedis", "invalid backend limits")
	}
	return store, nil
}
