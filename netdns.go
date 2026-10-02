package main

import (
	"context"
	"log"
	"net"
	"time"

	// Embed Mozilla's root CA set: Android/Termux keeps system
	// certificates where a Linux binary never looks, so without this
	// every TLS verification fails with "signed by unknown authority".
	_ "golang.org/x/crypto/x509roots/fallback"
)

// Android (Termux) has no /etc/resolv.conf, so Go's built-in resolver
// falls back to nameservers on [::1]:53 and 127.0.0.1:53 where nothing
// listens — every lookup fails with "connection refused", which
// surfaced as a failed EVE SSO token exchange on the phone build.
// Point the process-wide resolver at public DNS servers instead, and
// fall back to whatever server the system asked for if all of them are
// unreachable (some networks only allow their own resolver).
var dnsServers = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}

func init() {
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 5 * time.Second}
			var lastErr error
			for _, srv := range dnsServers {
				conn, err := d.DialContext(ctx, network, srv)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if address != "" {
				if conn, err := d.DialContext(ctx, network, address); err == nil {
					return conn, nil
				}
			}
			log.Printf("dns: all resolvers unreachable, last error: %v", lastErr)
			return nil, lastErr
		},
	}
}
