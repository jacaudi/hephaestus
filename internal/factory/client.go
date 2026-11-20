package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// maxReplyBytes bounds the reply to a registration that succeeded: the factory
// answers with an ID, so anything much larger is not a factory.
const maxReplyBytes = 1 << 20

// FileName is the name the factory gives the asset p of an image for arch:
// "kernel-amd64", "initramfs-amd64.xz" or "cmdline-metal-amd64". A part that is
// not one of the three yields "", which no URL serves.
func FileName(p fleet.TalosPart, arch fleet.Arch) string {
	switch p {
	case fleet.TalosKernel:
		return "kernel-" + string(arch)
	case fleet.TalosInitramfs:
		return "initramfs-" + string(arch) + ".xz"
	case fleet.TalosCmdline:
		return "cmdline-metal-" + string(arch)
	}
	return ""
}

// StatusError is a registration the factory answered with a status other than
// 200 and 201.
type StatusError struct {
	// Status is the HTTP status code.
	Status int
	// Body is the start of the response body as printable text, at most
	// fleet.MaxCauseBytes long: the factory says there why it refused a schematic.
	Body string
}

// Error implements error.
func (e *StatusError) Error() string {
	s := "the image factory answered " + strconv.Itoa(e.Status)
	if text := http.StatusText(e.Status); text != "" {
		s += " " + text
	}
	if e.Body != "" {
		s += ": " + e.Body
	}
	return s
}

// Client talks to one image factory.
type Client struct {
	base string
	hc   *http.Client
}

// New returns a Client for the factory at base, which has no trailing slash,
// sending requests through hc.
func New(base string, hc *http.Client) *Client {
	return &Client{base: base, hc: hc}
}

// URL is the address of the asset p of the image for schematic (an ID),
// version (for example "v1.12.11") and arch.
func (c *Client) URL(schematic, version string, arch fleet.Arch, p fleet.TalosPart) string {
	return c.base + "/image/" + schematic + "/" + version + "/" + FileName(p, arch)
}

// Register posts body, a schematic as YAML, to the factory and returns the ID
// it answers with. A status other than 200 and 201 is a *StatusError.
func (c *Client) Register(ctx context.Context, body []byte) (id string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/schematics", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("register the schematic: %w", err)
	}
	req.Header.Set("Content-Type", "application/yaml")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("register the schematic: %w", err)
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		text, rerr := io.ReadAll(io.LimitReader(resp.Body, fleet.MaxCauseBytes))
		se := &StatusError{Status: resp.StatusCode, Body: fleet.PrintableText(string(text))}
		if rerr != nil {
			return "", fmt.Errorf("register the schematic: %w (its body broke off: %w)", se, rerr)
		}
		return "", fmt.Errorf("register the schematic: %w", se)
	}

	var reply struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReplyBytes)).Decode(&reply); err != nil {
		return "", fmt.Errorf("register the schematic: decode the reply: %w", err)
	}
	if reply.ID == "" {
		return "", errors.New("register the schematic: the reply has no id")
	}
	return reply.ID, nil
}
