// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
// hephaestus: vendored from github.com/siderolabs/omni-infra-provider-bare-metal v0.15.0 (13d64d3ec8e1a4f90f50af160380b086030f2343), pkg/tftp/tftp_server.go. Changes: zap replaced by log/slog (levels unchanged); reads are served only to peers inside the provisioning subnet.

// Package tftp implements a TFTP server.
package tftp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"path"
	"strings"
	"time"

	"github.com/pin/tftp/v3"
	"golang.org/x/sync/errgroup"
)

// Server represents the TFTP server serving iPXE binaries from memory.
type Server struct {
	logger *slog.Logger

	files map[string][]byte

	listenAddress string
	subnet        netip.Prefix
}

// NewServer creates a new TFTP server serving the given files, keyed by the request path, to peers inside subnet.
func NewServer(listenAddress string, subnet netip.Prefix, files map[string][]byte, logger *slog.Logger) *Server {
	return &Server{
		listenAddress: listenAddress,
		subnet:        subnet,
		files:         files,
		logger:        logger,
	}
}

// Run runs the TFTP server.
func (s *Server) Run(ctx context.Context) error {
	readHandler := func(filename string, rf io.ReaderFrom) error {
		return s.handleRead(filename, rf)
	}

	srv := tftp.NewServer(readHandler, nil)

	// A standard TFTP server implementation receives requests on port 69 and
	// allocates a new high port (over 1024) dedicated to that request. In single
	// port mode, the same port is used for transmit and receive. If the server
	// is started on port 69, all communication will be done on port 69.
	// This option is required since the Kubernetes service definition defines a
	// single port.
	srv.EnableSinglePort()
	srv.SetTimeout(5 * time.Second)

	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		return srv.ListenAndServe(net.JoinHostPort(s.listenAddress, "69"))
	})

	eg.Go(func() error {
		<-ctx.Done()

		srv.Shutdown()

		return nil
	})

	return eg.Wait()
}

// handleRead is called when a client starts file download from server.
func (s *Server) handleRead(filename string, rf io.ReaderFrom) error {
	if !s.fromSubnet(rf) {
		s.logger.Warn("file request refused: peer outside the provisioning subnet", slog.String("filename", filename))

		return errors.New("request refused")
	}

	s.logger.Info("file requested", slog.String("filename", filename))

	// normalize the request lexically, so both "snp.efi" and "/amd64/../snp.efi" resolve to the same key
	name := strings.TrimPrefix(path.Clean("/"+filename), "/")

	contents, ok := s.files[name]
	if !ok {
		s.logger.Error("file not found", slog.String("filename", filename))

		return fmt.Errorf("file %q not found", filename)
	}

	n, err := rf.ReadFrom(bytes.NewReader(contents))
	if err != nil {
		s.logger.Error("failed to send file", slog.String("filename", filename), slog.Any("error", err))

		return err
	}

	s.logger.Info("file sent", slog.String("filename", filename), slog.Int64("bytes", n))

	return nil
}

// fromSubnet reports whether the peer of the transfer is inside the
// provisioning subnet. A transfer that does not name its peer is refused.
func (s *Server) fromSubnet(rf io.ReaderFrom) bool {
	t, ok := rf.(tftp.OutgoingTransfer)
	if !ok {
		return false
	}

	peer := t.RemoteAddr()

	addr, ok := netip.AddrFromSlice(peer.IP)

	return ok && s.subnet.Contains(addr.Unmap())
}
