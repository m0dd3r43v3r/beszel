package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	sshServer "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"
)

type ServerOptions struct {
	Addr    string
	Network string
	Keys    []ssh.PublicKey
}

func (a *Agent) StartServer(opts ServerOptions) error {
	sshServer.Handle(a.handleSession)

	slog.Info("Starting SSH server", "addr", opts.Addr, "network", opts.Network)

	if opts.Network == "unix" {
		// remove existing socket file if it exists
		if err := os.Remove(opts.Addr); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	// start listening on the address
	ln, err := net.Listen(opts.Network, opts.Addr)
	if err != nil {
		return err
	}
	defer ln.Close()

	// Start SSH server on the listener
	return sshServer.Serve(ln, nil, sshServer.NoPty(),
		sshServer.PublicKeyAuth(func(ctx sshServer.Context, key sshServer.PublicKey) bool {
			for _, pubKey := range opts.Keys {
				if sshServer.KeysEqual(key, pubKey) {
					return true
				}
			}
			return false
		}),
	)
}

func (a *Agent) handleSession(s sshServer.Session) {
	slog.Debug("New session", "client", s.RemoteAddr())
	stats := a.gatherStats(s.Context().SessionID())
	if err := json.NewEncoder(s).Encode(stats); err != nil {
		slog.Error("Error encoding stats", "err", err, "stats", stats)
		s.Exit(1)
	}
	s.Exit(0)
}

// ParseKeys parses a string containing SSH public keys in authorized_keys format.
// It returns a slice of ssh.PublicKey and an error if any key fails to parse.
func ParseKeys(input string) ([]ssh.PublicKey, error) {
	var parsedKeys []ssh.PublicKey
	for _, line := range strings.Split(input, "\n") {
		line = strings.TrimSpace(line)
		// Skip empty lines or comments
		if len(line) == 0 || strings.HasPrefix(line, "#") {
			continue
		}
		// Parse the key
		parsedKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("failed to parse key: %s, error: %w", line, err)
		}
		parsedKeys = append(parsedKeys, parsedKey)
	}
	return parsedKeys, nil
}

// GetAddress gets the address to listen on or connect to from environment variables or default value.
func GetAddress(addr string) string {
	if addr == "" {
		addr, _ = GetEnv("LISTEN")
	}
	if addr == "" {
		// Legacy PORT environment variable support
		addr, _ = GetEnv("PORT")
	}
	if addr == "" {
		return ":45876"
	}
	// prefix with : if only port was provided
	if GetNetwork(addr) != "unix" && !strings.Contains(addr, ":") {
		addr = ":" + addr
	}
	return addr
}

// GetNetwork returns the network type to use based on the address
func GetNetwork(addr string) string {
	if network, ok := GetEnv("NETWORK"); ok && network != "" {
		return network
	}
	if strings.HasPrefix(addr, "/") {
		return "unix"
	}
	return "tcp"
}
