//go:build linux

package guestagent

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"math/big"
)

// Host key types the agent generates, by ssh_host_<type>_key file name.
const (
	keyEd25519 = "ed25519"
	keyECDSA   = "ecdsa"
	keyRSA     = "rsa"
)

// newHostKey generates a host key of kind (ed25519, ecdsa = P-256, rsa =
// 3072 bits) and returns it in OpenSSH formats: the unencrypted
// "openssh-key-v1" private key (PEM) and the authorized_keys style public
// line. Done in-process so rotation needs no ssh-keygen (ed25519 and P-256
// take well under a millisecond) and tests exercise the real encoding.
func newHostKey(kind, comment string) (priv, pub []byte, err error) {
	var keyType string
	var blob, secret []byte // public blob; private fields after the type
	switch kind {
	case keyEd25519:
		pk, sk, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		keyType = "ssh-ed25519"
		blob = sshString(sshString(nil, []byte(keyType)), pk)
		secret = sshString(sshString(nil, pk), sk) // sk is seed || public
	case keyECDSA:
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		q, err := k.PublicKey.Bytes() // uncompressed point
		if err != nil {
			return nil, nil, err
		}
		d, err := k.Bytes()
		if err != nil {
			return nil, nil, err
		}
		keyType = "ecdsa-sha2-nistp256"
		blob = sshString(sshString(sshString(nil, []byte(keyType)), []byte("nistp256")), q)
		secret = sshMpint(sshString(sshString(nil, []byte("nistp256")), q), new(big.Int).SetBytes(d))
	case keyRSA:
		k, err := rsa.GenerateKey(rand.Reader, 3072)
		if err != nil {
			return nil, nil, err
		}
		e := big.NewInt(int64(k.E))
		keyType = "ssh-rsa"
		blob = sshMpint(sshMpint(sshString(nil, []byte(keyType)), e), k.N)
		secret = sshMpint(sshMpint(sshMpint(sshMpint(sshMpint(sshMpint(nil, k.N), e), k.D), k.Precomputed.Qinv), k.Primes[0]), k.Primes[1])
	default:
		return nil, nil, fmt.Errorf("unsupported host key type %q", kind)
	}

	var check [4]byte
	if _, err := rand.Read(check[:]); err != nil {
		return nil, nil, err
	}
	var sec []byte
	sec = append(sec, check[:]...)
	sec = append(sec, check[:]...)
	sec = sshString(sec, []byte(keyType))
	sec = append(sec, secret...)
	sec = sshString(sec, []byte(comment))
	for i := byte(1); len(sec)%8 != 0; i++ {
		sec = append(sec, i)
	}

	body := []byte("openssh-key-v1\x00")
	body = sshString(body, []byte("none")) // cipher
	body = sshString(body, []byte("none")) // kdf
	body = sshString(body, nil)            // kdf options
	body = binary.BigEndian.AppendUint32(body, 1)
	body = sshString(body, blob)
	body = sshString(body, sec)

	priv = pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: body})
	pub = []byte(keyType + " " + base64.StdEncoding.EncodeToString(blob) + " " + comment + "\n")
	return priv, pub, nil
}

func sshString(b, s []byte) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

// sshMpint appends an SSH mpint (RFC 4251): big-endian, minimal, with a
// leading zero byte when the high bit is set. Values here are positive.
func sshMpint(b []byte, n *big.Int) []byte {
	v := n.Bytes()
	if len(v) > 0 && v[0]&0x80 != 0 {
		v = append([]byte{0}, v...)
	}
	return sshString(b, v)
}
