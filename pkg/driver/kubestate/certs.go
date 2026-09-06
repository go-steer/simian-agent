// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubestate

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// certLifetime is how long a synthesized certificate claims to have been valid
// for: notBefore is notAfter minus this.
//
// Ninety days, the Let's Encrypt term, because the diagnosis reads off the
// whole validity window and not just its end. A certificate expiring in two
// days that was issued three minutes ago is a certificate someone just made,
// which tells a subject it is looking at a rig.
//
// Dating from notAfter rather than from now is also what keeps a certificate
// with a negative window coherent. "Expired two days ago" has to mean issued
// before it expired; anchoring notBefore to the clock instead would produce a
// certificate whose validity started after it ended, which is not a stale
// certificate but a malformed one.
const certLifetime = 90 * 24 * time.Hour

// certBackdate is the smallest gap between now and notBefore.
//
// It caps the rule above for a long-lived certificate — one valid for a year
// would otherwise claim a notBefore nine months in the future — and it absorbs
// clock skew between whatever generated the certificate and whatever reads it.
// A certificate whose validity starts in the future is "not yet valid", which
// is a different diagnosis than the one this kind poses.
const certBackdate = time.Hour

// certSerialBits is the width of the random serial number. RFC 5280 wants a
// positive integer of at most 20 octets; 128 bits is the conventional choice
// and leaves room for the sign bit.
const certSerialBits = 128

// expiringCert is a self-signed TLS certificate and its private key, PEM
// encoded, ready to be the two halves of a kubernetes.io/tls Secret.
type expiringCert struct {
	certPEM   []byte
	keyPEM    []byte
	notBefore time.Time

	// notAfter is what the fault is about. Returned rather than recomputed by
	// the caller so a test can assert on the certificate that was actually
	// generated instead of on the arithmetic that was supposed to produce it.
	notAfter time.Time
}

// newExpiringCert issues a self-signed certificate for commonName that expires
// validFor from now — which may be in the past, and usefully so: a certificate
// that expired last week is a real diagnosis and a different one from a
// certificate expiring on Thursday.
//
// Self-signed rather than issued from a synthesized CA. A chain would be more
// realistic and would double the objects for no gain: nothing in the arena
// verifies this certificate, and the diagnosis the fault poses is read off
// notAfter, which a leaf carries on its own.
//
// ECDSA P-256 rather than RSA-2048 because key generation is the slow part of
// Apply for this kind, and P-256 is roughly a millisecond against a few hundred
// for RSA — Apply holds up the fault's own lease while it runs.
func newExpiringCert(commonName string, validFor time.Duration, now time.Time) (expiringCert, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return expiringCert{}, fmt.Errorf("generate key: %w", err)
	}
	serialMax := new(big.Int).Lsh(big.NewInt(1), certSerialBits)
	serial, err := rand.Int(rand.Reader, serialMax)
	if err != nil {
		return expiringCert{}, fmt.Errorf("generate serial: %w", err)
	}

	notAfter := now.Add(validFor)
	notBefore := notAfter.Add(-certLifetime)
	if latest := now.Add(-certBackdate); notBefore.After(latest) {
		notBefore = latest
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// The SAN as well as the CN. Every TLS implementation shipped this
		// decade ignores the CN entirely, so a certificate without a matching
		// SAN would fail verification for the wrong reason — hostname mismatch
		// rather than expiry — if anything in the scenario ever did verify it.
		DNSNames:              []string{commonName},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return expiringCert{}, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return expiringCert{}, fmt.Errorf("marshal key: %w", err)
	}

	return expiringCert{
		certPEM:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:    pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		notBefore: notBefore,
		notAfter:  notAfter,
	}, nil
}
