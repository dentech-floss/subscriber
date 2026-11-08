package subscriber

import (
	"errors"
	"fmt"
	"os"

	"github.com/nats-io/nats.go"
	"go.yaml.in/yaml/v3"
)

// NATSConfig is how the subscriber connects to NATS and finds its consumers.
// Every empty field is read from the environment variable named in its
// comment.
type NATSConfig struct {
	// URL of the NATS server, e.g. tls://host:4222. NATS_URL.
	URL string
	// CAFile is a PEM file with the CA that signed the server's
	// certificate. Optional: without it, the system roots are used.
	// NATS_CA_FILE.
	CAFile string
	// User and Password log in with a user from the server's config.
	// NATS_USER, NATS_PASSWORD.
	User     string
	Password string
	// CredsFile logs in with a JWT and NKey seed (operator mode) instead
	// of User and Password. NATS_CREDS_FILE.
	CredsFile string
	// MappingFile is the YAML file that maps each of the service's push
	// URLs to a JetStream stream and durable consumer. NATS_MAPPING_FILE.
	MappingFile string
}

func (c *NATSConfig) setDefaults() {
	fromEnv := func(field *string, name string) {
		if *field == "" {
			*field = os.Getenv(name)
		}
	}
	fromEnv(&c.URL, "NATS_URL")
	fromEnv(&c.CAFile, "NATS_CA_FILE")
	fromEnv(&c.User, "NATS_USER")
	fromEnv(&c.Password, "NATS_PASSWORD")
	fromEnv(&c.CredsFile, "NATS_CREDS_FILE")
	fromEnv(&c.MappingFile, "NATS_MAPPING_FILE")
}

func (c *NATSConfig) validate() error {
	var errs []error
	if c.URL == "" {
		errs = append(errs, errors.New("NATS_URL is not set"))
	}
	if c.MappingFile == "" {
		errs = append(errs, errors.New("NATS_MAPPING_FILE is not set"))
	}
	if c.CredsFile != "" && c.User != "" {
		errs = append(errs, errors.New("set either NATS_CREDS_FILE or NATS_USER, not both"))
	}
	if c.User != "" && c.Password == "" {
		errs = append(errs, errors.New("NATS_USER is set but NATS_PASSWORD is empty"))
	}
	return errors.Join(errs...)
}

// connect opens a connection that reconnects forever: a NATS restart must
// not stop the service.
func (c *NATSConfig) connect(name string) (*nats.Conn, error) {
	opts := []nats.Option{
		nats.Name(name),
		nats.MaxReconnects(-1),
	}
	if c.CAFile != "" {
		opts = append(opts, nats.RootCAs(c.CAFile))
	}
	if c.User != "" {
		opts = append(opts, nats.UserInfo(c.User, c.Password))
	}
	if c.CredsFile != "" {
		opts = append(opts, nats.UserCredentials(c.CredsFile))
	}
	nc, err := nats.Connect(c.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS at %s: %w", c.URL, err)
	}
	return nc, nil
}

// natsMapping is the part of the mapping file that the subscriber reads. The
// same file holds the publisher's subjects and the consumer settings that
// OpenTofu uses to create the streams and consumers; this library ignores
// those fields and reads the settings from the server instead.
type natsMapping struct {
	Consumers []natsBinding `yaml:"consumers"`
}

// natsBinding ties one push URL of one service to a durable consumer.
type natsBinding struct {
	Service string `yaml:"service"`
	URL     string `yaml:"url"`
	Stream  string `yaml:"stream"`
	Durable string `yaml:"durable"`
}

// dlqSubject is where messages go that fail on their last delivery.
func (b natsBinding) dlqSubject() string {
	return "dlq." + b.Service + "." + b.Durable
}

func loadNATSMapping(path string) (*natsMapping, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the NATS mapping: %w", err)
	}
	var m natsMapping
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing the NATS mapping %s: %w", path, err)
	}
	seen := map[[2]string]bool{}
	for i, b := range m.Consumers {
		if b.Service == "" || b.URL == "" || b.Stream == "" || b.Durable == "" {
			return nil, fmt.Errorf("NATS mapping %s: consumer %d needs service, url, stream and durable", path, i)
		}
		key := [2]string{b.Service, b.URL}
		if seen[key] {
			return nil, fmt.Errorf("NATS mapping %s: %s %s is mapped twice", path, b.Service, b.URL)
		}
		seen[key] = true
	}
	return &m, nil
}

func (m *natsMapping) binding(service, url string) (natsBinding, bool) {
	for _, b := range m.Consumers {
		if b.Service == service && b.URL == url {
			return b, true
		}
	}
	return natsBinding{}, false
}
