package k8s

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"math/big"
)

// GenerateSSHKeyPair returns an OpenSSH-compatible keypair: a PKCS#1 PEM
// private key (which the ssh client reads directly) and an authorized_keys
// line.
//
// Stdlib only, deliberately: the orchestrator image is FROM scratch (no
// ssh-keygen to shell out to) and golang.org/x/crypto is not a direct
// dependency of this module.
func GenerateSSHKeyPair(comment string) (privPEM []byte, authorizedKey []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, nil, fmt.Errorf("generating RSA key: %w", err)
	}
	privPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	// RFC 4253 "ssh-rsa" public key blob: string "ssh-rsa", mpint e, mpint n.
	blob := append(sshString([]byte("ssh-rsa")), sshMPInt(big.NewInt(int64(key.E)))...)
	blob = append(blob, sshMPInt(key.N)...)

	line := "ssh-rsa " + base64.StdEncoding.EncodeToString(blob)
	if comment != "" {
		line += " " + comment
	}
	return privPEM, []byte(line), nil
}

func sshString(b []byte) []byte {
	out := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	return out
}

// sshMPInt encodes a positive big.Int as an SSH mpint: big-endian two's
// complement, so a leading 0x00 is required when the high bit is set.
func sshMPInt(n *big.Int) []byte {
	b := n.Bytes()
	if len(b) > 0 && b[0]&0x80 != 0 {
		b = append([]byte{0x00}, b...)
	}
	return sshString(b)
}
