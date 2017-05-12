// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tls

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"io"
	"math/big"

	"golang_org/x/crypto/curve25519"
)

var errClientKeyExchange = errors.New("tls: invalid ClientKeyExchange message")
var errServerKeyExchange = errors.New("tls: invalid ServerKeyExchange message")

// rsaKeyAgreement implements the standard TLS key agreement where the client
// encrypts the pre-master secret to the server's public key.
type rsaKeyAgreement struct{}

func (ka rsaKeyAgreement) generateServerKeyExchange(config *Config, cert *Certificate, clientHello *clientHelloMsg, hello *serverHelloMsg) (*serverKeyExchangeMsg, error) {
	return nil, nil
}

func (ka rsaKeyAgreement) processClientKeyExchange(config *Config, cert *Certificate, ckx *clientKeyExchangeMsg, version uint16) ([]byte, error) {
	if len(ckx.ciphertext) < 2 {
		return nil, errClientKeyExchange
	}

	ciphertext := ckx.ciphertext
	if version != VersionSSL30 {
		ciphertextLen := int(ckx.ciphertext[0])<<8 | int(ckx.ciphertext[1])
		if ciphertextLen != len(ckx.ciphertext)-2 {
			return nil, errClientKeyExchange
		}
		ciphertext = ckx.ciphertext[2:]
	}
	priv, ok := cert.PrivateKey.(crypto.Decrypter)
	if !ok {
		return nil, errors.New("tls: certificate private key does not implement crypto.Decrypter")
	}
	// Perform constant time RSA PKCS#1 v1.5 decryption
	preMasterSecret, err := priv.Decrypt(config.rand(), ciphertext, &rsa.PKCS1v15DecryptOptions{SessionKeyLen: 48})
	if err != nil {
		return nil, err
	}
	// We don't check the version number in the premaster secret. For one,
	// by checking it, we would leak information about the validity of the
	// encrypted pre-master secret. Secondly, it provides only a small
	// benefit against a downgrade attack and some implementations send the
	// wrong version anyway. See the discussion at the end of section
	// 7.4.7.1 of RFC 4346.
	return preMasterSecret, nil
}

func (ka rsaKeyAgreement) processServerKeyExchange(config *Config, clientHello *clientHelloMsg, serverHello *serverHelloMsg, cert *x509.Certificate, skx *serverKeyExchangeMsg) error {
	return errors.New("tls: unexpected ServerKeyExchange")
}

func (ka rsaKeyAgreement) generateClientKeyExchange(config *Config, clientHello *clientHelloMsg, cert *x509.Certificate) ([]byte, *clientKeyExchangeMsg, error) {
	preMasterSecret := make([]byte, 48)
	preMasterSecret[0] = byte(clientHello.vers >> 8)
	preMasterSecret[1] = byte(clientHello.vers)
	_, err := io.ReadFull(config.rand(), preMasterSecret[2:])
	if err != nil {
		return nil, nil, err
	}

	encrypted, err := rsa.EncryptPKCS1v15(config.rand(), cert.PublicKey.(*rsa.PublicKey), preMasterSecret)
	if err != nil {
		return nil, nil, err
	}
	ckx := new(clientKeyExchangeMsg)
	ckx.ciphertext = make([]byte, len(encrypted)+2)
	ckx.ciphertext[0] = byte(len(encrypted) >> 8)
	ckx.ciphertext[1] = byte(len(encrypted))
	copy(ckx.ciphertext[2:], encrypted)
	return preMasterSecret, ckx, nil
}

// sha1Hash calculates a SHA1 hash over the given byte slices.
func sha1Hash(slices [][]byte) []byte {
	hsha1 := sha1.New()
	for _, slice := range slices {
		hsha1.Write(slice)
	}
	return hsha1.Sum(nil)
}

// md5SHA1Hash implements TLS 1.0's hybrid hash function which consists of the
// concatenation of an MD5 and SHA1 hash.
func md5SHA1Hash(slices [][]byte) []byte {
	md5sha1 := make([]byte, md5.Size+sha1.Size)
	hmd5 := md5.New()
	for _, slice := range slices {
		hmd5.Write(slice)
	}
	copy(md5sha1, hmd5.Sum(nil))
	copy(md5sha1[md5.Size:], sha1Hash(slices))
	return md5sha1
}

// hashForServerKeyExchange hashes the given slices and returns their digest
// and the identifier of the hash function used. The sigAndHash argument is
// only used for >= TLS 1.2 and precisely identifies the hash function to use.
func hashForServerKeyExchange(sigAndHash signatureAndHash, version uint16, slices ...[]byte) ([]byte, crypto.Hash, error) {
	if version >= VersionTLS12 {
		if !isSupportedSignatureAndHash(sigAndHash, supportedSignatureAlgorithms) {
			return nil, crypto.Hash(0), errors.New("tls: unsupported hash function used by peer")
		}
		hashFunc, err := lookupTLSHash(sigAndHash.hash)
		if err != nil {
			return nil, crypto.Hash(0), err
		}
		h := hashFunc.New()
		for _, slice := range slices {
			h.Write(slice)
		}
		digest := h.Sum(nil)
		return digest, hashFunc, nil
	}
	if sigAndHash.signature == signatureECDSA {
		return sha1Hash(slices), crypto.SHA1, nil
	}
	return md5SHA1Hash(slices), crypto.MD5SHA1, nil
}

// pickTLS12HashForSignature returns a TLS 1.2 hash identifier for signing a
// ServerKeyExchange given the signature type being used and the client's
// advertised list of supported signature and hash combinations.
func pickTLS12HashForSignature(sigType uint8, clientList []signatureAndHash) (uint8, error) {
	if len(clientList) == 0 {
		// If the client didn't specify any signature_algorithms
		// extension then we can assume that it supports SHA1. See
		// http://tools.ietf.org/html/rfc5246#section-7.4.1.4.1
		return hashSHA1, nil
	}

	for _, sigAndHash := range clientList {
		if sigAndHash.signature != sigType {
			continue
		}
		if isSupportedSignatureAndHash(sigAndHash, supportedSignatureAlgorithms) {
			return sigAndHash.hash, nil
		}
	}

	return 0, errors.New("tls: client doesn't support any common hash functions")
}

func curveForCurveID(id CurveID) (elliptic.Curve, bool) {
	switch id {
	case CurveP256:
		return elliptic.P256(), true
	case CurveP384:
		return elliptic.P384(), true
	case CurveP521:
		return elliptic.P521(), true
	default:
		return nil, false
	}

}

// ecdheRSAKeyAgreement implements a TLS key agreement where the server
// generates a ephemeral EC public/private key pair and signs it. The
// pre-master secret is then calculated using ECDH. The signature may
// either be ECDSA or RSA.
type ecdheKeyAgreement struct {
	version    uint16
	sigType    uint8
	privateKey []byte
	curveid    CurveID

	// publicKey is used to store the peer's public value when X25519 is
	// being used.
	publicKey []byte
	// x and y are used to store the peer's public value when one of the
	// NIST curves is being used.
	x, y *big.Int
}

func (ka *ecdheKeyAgreement) generateServerKeyExchange(config *Config, cert *Certificate, clientHello *clientHelloMsg, hello *serverHelloMsg) (*serverKeyExchangeMsg, error) {
	preferredCurves := config.curvePreferences()

NextCandidate:
	for _, candidate := range preferredCurves {
		for _, c := range clientHello.supportedCurves {
			if candidate == c {
				ka.curveid = c
				break NextCandidate
			}
		}
	}

	if ka.curveid == 0 {
		return nil, errors.New("tls: no supported elliptic curves offered")
	}

	var ecdhePublic []byte

	if ka.curveid == X25519 {
		var scalar, public [32]byte
		if _, err := io.ReadFull(config.rand(), scalar[:]); err != nil {
			return nil, err
		}

		curve25519.ScalarBaseMult(&public, &scalar)
		ka.privateKey = scalar[:]
		ecdhePublic = public[:]
	} else {
		curve, ok := curveForCurveID(ka.curveid)
		if !ok {
			return nil, errors.New("tls: preferredCurves includes unsupported curve")
		}

		var x, y *big.Int
		var err error
		ka.privateKey, x, y, err = elliptic.GenerateKey(curve, config.rand())
		if err != nil {
			return nil, err
		}
		ecdhePublic = elliptic.Marshal(curve, x, y)
	}

	// http://tools.ietf.org/html/rfc4492#section-5.4
	serverECDHParams := make([]byte, 1+2+1+len(ecdhePublic))
	serverECDHParams[0] = 3 // named curve
	serverECDHParams[1] = byte(ka.curveid >> 8)
	serverECDHParams[2] = byte(ka.curveid)
	serverECDHParams[3] = byte(len(ecdhePublic))
	copy(serverECDHParams[4:], ecdhePublic)

	sigAndHash := signatureAndHash{signature: ka.sigType}

	if ka.version >= VersionTLS12 {
		var err error
		if sigAndHash.hash, err = pickTLS12HashForSignature(ka.sigType, clientHello.signatureAndHashes); err != nil {
			return nil, err
		}
	}

	digest, hashFunc, err := hashForServerKeyExchange(sigAndHash, ka.version, clientHello.random, hello.random, serverECDHParams)
	if err != nil {
		return nil, err
	}

	priv, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("tls: certificate private key does not implement crypto.Signer")
	}
	var sig []byte
	switch ka.sigType {
	case signatureECDSA:
		_, ok := priv.Public().(*ecdsa.PublicKey)
		if !ok {
			return nil, errors.New("tls: ECDHE ECDSA requires an ECDSA server key")
		}
	case signatureRSA:
		_, ok := priv.Public().(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("tls: ECDHE RSA requires a RSA server key")
		}
	default:
		return nil, errors.New("tls: unknown ECDHE signature algorithm")
	}
	sig, err = priv.Sign(config.rand(), digest, hashFunc)
	if err != nil {
		return nil, errors.New("tls: failed to sign ECDHE parameters: " + err.Error())
	}

	skx := new(serverKeyExchangeMsg)
	sigAndHashLen := 0
	if ka.version >= VersionTLS12 {
		sigAndHashLen = 2
	}
	skx.key = make([]byte, len(serverECDHParams)+sigAndHashLen+2+len(sig))
	copy(skx.key, serverECDHParams)
	k := skx.key[len(serverECDHParams):]
	if ka.version >= VersionTLS12 {
		k[0] = sigAndHash.hash
		k[1] = sigAndHash.signature
		k = k[2:]
	}
	k[0] = byte(len(sig) >> 8)
	k[1] = byte(len(sig))
	copy(k[2:], sig)

	return skx, nil
}

func (ka *ecdheKeyAgreement) processClientKeyExchange(config *Config, cert *Certificate, ckx *clientKeyExchangeMsg, version uint16) ([]byte, error) {
	if len(ckx.ciphertext) == 0 || int(ckx.ciphertext[0]) != len(ckx.ciphertext)-1 {
		return nil, errClientKeyExchange
	}

	if ka.curveid == X25519 {
		if len(ckx.ciphertext) != 1+32 {
			return nil, errClientKeyExchange
		}

		var theirPublic, sharedKey, scalar [32]byte
		copy(theirPublic[:], ckx.ciphertext[1:])
		copy(scalar[:], ka.privateKey)
		curve25519.ScalarMult(&sharedKey, &scalar, &theirPublic)
		return sharedKey[:], nil
	}

	curve, ok := curveForCurveID(ka.curveid)
	if !ok {
		panic("internal error")
	}
	x, y := elliptic.Unmarshal(curve, ckx.ciphertext[1:])
	if x == nil {
		return nil, errClientKeyExchange
	}
	if !curve.IsOnCurve(x, y) {
		return nil, errClientKeyExchange
	}
	x, _ = curve.ScalarMult(x, y, ka.privateKey)
	preMasterSecret := make([]byte, (curve.Params().BitSize+7)>>3)
	xBytes := x.Bytes()
	copy(preMasterSecret[len(preMasterSecret)-len(xBytes):], xBytes)

	return preMasterSecret, nil
}

func (ka *ecdheKeyAgreement) processServerKeyExchange(config *Config, clientHello *clientHelloMsg, serverHello *serverHelloMsg, cert *x509.Certificate, skx *serverKeyExchangeMsg) error {
	if len(skx.key) < 4 {
		return errServerKeyExchange
	}
	if skx.key[0] != 3 { // named curve
		return errors.New("tls: server selected unsupported curve")
	}
	ka.curveid = CurveID(skx.key[1])<<8 | CurveID(skx.key[2])

	publicLen := int(skx.key[3])
	if publicLen+4 > len(skx.key) {
		return errServerKeyExchange
	}
	serverECDHParams := skx.key[:4+publicLen]
	publicKey := serverECDHParams[4:]

	sig := skx.key[4+publicLen:]
	if len(sig) < 2 {
		return errServerKeyExchange
	}

	if ka.curveid == X25519 {
		if len(publicKey) != 32 {
			return errors.New("tls: bad X25519 public value")
		}
		ka.publicKey = publicKey
	} else {
		curve, ok := curveForCurveID(ka.curveid)
		if !ok {
			return errors.New("tls: server selected unsupported curve")
		}

		ka.x, ka.y = elliptic.Unmarshal(curve, publicKey)
		if ka.x == nil {
			return errServerKeyExchange
		}
		if !curve.IsOnCurve(ka.x, ka.y) {
			return errServerKeyExchange
		}
	}

	sigAndHash := signatureAndHash{signature: ka.sigType}
	if ka.version >= VersionTLS12 {
		// handle SignatureAndHashAlgorithm
		sigAndHash = signatureAndHash{hash: sig[0], signature: sig[1]}
		if sigAndHash.signature != ka.sigType {
			return errServerKeyExchange
		}
		sig = sig[2:]
		if len(sig) < 2 {
			return errServerKeyExchange
		}
	}
	sigLen := int(sig[0])<<8 | int(sig[1])
	if sigLen+2 != len(sig) {
		return errServerKeyExchange
	}
	sig = sig[2:]

	digest, hashFunc, err := hashForServerKeyExchange(sigAndHash, ka.version, clientHello.random, serverHello.random, serverECDHParams)
	if err != nil {
		return err
	}
	switch ka.sigType {
	case signatureECDSA:
		pubKey, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("tls: ECDHE ECDSA requires a ECDSA server public key")
		}
		ecdsaSig := new(ecdsaSignature)
		if _, err := asn1.Unmarshal(sig, ecdsaSig); err != nil {
			return err
		}
		if ecdsaSig.R.Sign() <= 0 || ecdsaSig.S.Sign() <= 0 {
			return errors.New("tls: ECDSA signature contained zero or negative values")
		}
		if !ecdsa.Verify(pubKey, digest, ecdsaSig.R, ecdsaSig.S) {
			return errors.New("tls: ECDSA verification failure")
		}
	case signatureRSA:
		pubKey, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return errors.New("tls: ECDHE RSA requires a RSA server public key")
		}
		if err := rsa.VerifyPKCS1v15(pubKey, hashFunc, digest, sig); err != nil {
			return err
		}
	default:
		return errors.New("tls: unknown ECDHE signature algorithm")
	}

	return nil
}

func (ka *ecdheKeyAgreement) generateClientKeyExchange(config *Config, clientHello *clientHelloMsg, cert *x509.Certificate) ([]byte, *clientKeyExchangeMsg, error) {
	if ka.curveid == 0 {
		return nil, nil, errors.New("tls: missing ServerKeyExchange message")
	}

	var serialized, preMasterSecret []byte

	if ka.curveid == X25519 {
		var ourPublic, theirPublic, sharedKey, scalar [32]byte

		if _, err := io.ReadFull(config.rand(), scalar[:]); err != nil {
			return nil, nil, err
		}

		copy(theirPublic[:], ka.publicKey)
		curve25519.ScalarBaseMult(&ourPublic, &scalar)
		curve25519.ScalarMult(&sharedKey, &scalar, &theirPublic)
		serialized = ourPublic[:]
		preMasterSecret = sharedKey[:]
	} else {
		curve, ok := curveForCurveID(ka.curveid)
		if !ok {
			panic("internal error")
		}
		priv, mx, my, err := elliptic.GenerateKey(curve, config.rand())
		if err != nil {
			return nil, nil, err
		}
		x, _ := curve.ScalarMult(ka.x, ka.y, priv)
		preMasterSecret = make([]byte, (curve.Params().BitSize+7)>>3)
		xBytes := x.Bytes()
		copy(preMasterSecret[len(preMasterSecret)-len(xBytes):], xBytes)

		serialized = elliptic.Marshal(curve, mx, my)
	}

	ckx := new(clientKeyExchangeMsg)
	ckx.ciphertext = make([]byte, 1+len(serialized))
	ckx.ciphertext[0] = byte(len(serialized))
	copy(ckx.ciphertext[1:], serialized)

	return preMasterSecret, ckx, nil
}

type dheKeyAgreement struct {
	version  uint16
	sigType  uint8
	p, g, Ys *big.Int // stuff stored in ka by client. Ys is server's pubkey
	x        *big.Int // stuff stored in ka by server. x is server's private key
}

var bigOne = big.NewInt(1)

func (ka *dheKeyAgreement) generateServerKeyExchange(config *Config, cert *Certificate, clientHello *clientHelloMsg, hello *serverHelloMsg) (*serverKeyExchangeMsg, error) {
	if config.DhParamP == nil || config.DhParamG == nil {
		return nil, errors.New("tls: config is missing DH params needed for DHE ciphersuite")
	}

	pBytes := config.DhParamP.Bytes()
	lenPBytes := len(pBytes)
	gBytes := config.DhParamG.Bytes()
	lenGBytes := len(gBytes)

	// create a private key based on p and g
	pMinus1 := new(big.Int).Sub(config.DhParamP, bigOne)
	for {
		var err error
		if ka.x, err = rand.Int(config.rand(), pMinus1); err != nil {
			return nil, err
		}
		if ka.x.Sign() > 0 {
			break
		}
	}

	// create a public key
	pubKey := new(big.Int).Exp(config.DhParamG, ka.x, config.DhParamP)
	pubKeyBytes := pubKey.Bytes()
	lenPubKeyBytes := len(pubKeyBytes)

	lenServerDHParams := 2 + lenPBytes + 2 + lenGBytes + 2 + lenPubKeyBytes
	serverDHParams := make([]byte, lenServerDHParams)

	// pack the serverDHparams
	serverDHParams[0] = byte(lenPBytes >> 8)
	serverDHParams[1] = byte(lenPBytes)
	copy(serverDHParams[2:], pBytes)

	gLenOffset := 2 + lenPBytes
	serverDHParams[gLenOffset] = byte(lenGBytes >> 8)
	serverDHParams[gLenOffset+1] = byte(lenGBytes)
	copy(serverDHParams[gLenOffset+2:], gBytes)

	pubKeyLenOffset := gLenOffset + 2 + lenGBytes
	serverDHParams[pubKeyLenOffset] = byte(lenPubKeyBytes >> 8)
	serverDHParams[pubKeyLenOffset+1] = byte(lenPubKeyBytes)
	copy(serverDHParams[pubKeyLenOffset+2:], pubKeyBytes)

	//sign the serverDHParams
	sigAndHash := signatureAndHash{signature: ka.sigType}

	if ka.version >= VersionTLS12 {
		var err error
		if sigAndHash.hash, err = pickTLS12HashForSignature(ka.sigType, clientHello.signatureAndHashes); err != nil {
			return nil, err
		}
	}

	digest, hashFunc, err := hashForServerKeyExchange(sigAndHash, ka.version, clientHello.random, hello.random, serverDHParams)
	if err != nil {
		return nil, err
	}

	priv, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("tls: certificate private key does not implement crypto.Signer")
	}
	var sig []byte
	switch ka.sigType {
	/* // Currently there are no ciphersuites implemented in crypto/tls that use DHE and ECDSA
	   case signatureECDSA:
	       _, ok := priv.Public().(*ecdsa.PublicKey)
	       if !ok {
	           return nil, errors.New("tls: DHE ECDSA requires an ECDSA server key")
	       }
	*/
	case signatureRSA:
		_, ok := priv.Public().(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("tls: DHE RSA requires a RSA server key")
		}
	default:
		return nil, errors.New("tls: unknown DHE signature algorithm")
	}
	sig, err = priv.Sign(config.rand(), digest, hashFunc)
	if err != nil {
		return nil, errors.New("tls: failed to sign DHE parameters: " + err.Error())
	}

	skx := new(serverKeyExchangeMsg)
	sigAndHashLen := 0
	if ka.version >= VersionTLS12 {
		sigAndHashLen = 2
	}
	skx.key = make([]byte, len(serverDHParams)+sigAndHashLen+2+len(sig))
	copy(skx.key, serverDHParams)
	k := skx.key[len(serverDHParams):]
	if ka.version >= VersionTLS12 {
		k[0] = sigAndHash.hash
		k[1] = sigAndHash.signature
		k = k[2:]
	}
	k[0] = byte(len(sig) >> 8)
	k[1] = byte(len(sig))
	copy(k[2:], sig)

	return skx, nil
}

func (ka *dheKeyAgreement) processClientKeyExchange(config *Config, cert *Certificate, ckx *clientKeyExchangeMsg, version uint16) ([]byte, error) {
	if len(ckx.ciphertext) < 2 {
		return nil, errClientKeyExchange
	}
	clientPubKeyLen := int(ckx.ciphertext[0])<<8 | int(ckx.ciphertext[1])

	if len(ckx.ciphertext) != 2+clientPubKeyLen {
		return nil, errClientKeyExchange
	}

	clientPubKey := new(big.Int).SetBytes(ckx.ciphertext[2:])

	pMinus1 := new(big.Int).Sub(config.DhParamP, bigOne)

	if clientPubKey.Cmp(bigOne) <= 0 || clientPubKey.Cmp(pMinus1) >= 0 {
		return nil, errors.New("tls: Client DH parameter out of bounds")
	}
	preMasterSecret := new(big.Int).Exp(clientPubKey, ka.x, config.DhParamP).Bytes()
	return preMasterSecret, nil

}

func (ka *dheKeyAgreement) processServerKeyExchange(config *Config, clientHello *clientHelloMsg, serverHello *serverHelloMsg, cert *x509.Certificate, skx *serverKeyExchangeMsg) error {
	if len(skx.key) < 2 {
		return errServerKeyExchange
	}
	pLen := int(skx.key[0])<<8 | int(skx.key[1])

	if 2+pLen > len(skx.key) {
		return errServerKeyExchange
	}
	serverP := skx.key[2 : pLen+2]

	if 2+pLen+2 > len(skx.key) {
		return errServerKeyExchange
	}
	gLenOffset := 2 + pLen
	gLen := int(skx.key[gLenOffset])<<8 | int(skx.key[gLenOffset+1])

	if 2+pLen+2+gLen > len(skx.key) {
		return errServerKeyExchange
	}
	serverG := skx.key[gLenOffset+2 : gLenOffset+2+gLen]

	if 2+pLen+2+gLen+2 > len(skx.key) {
		return errServerKeyExchange
	}
	pubKeyLenOffset := 2 + gLenOffset + gLen
	pubKeyLen := int(skx.key[pubKeyLenOffset])<<8 | int(skx.key[pubKeyLenOffset+1])

	if 2+pLen+2+gLen+2+pubKeyLen > len(skx.key) {
		return errServerKeyExchange
	}
	serverPubKey := skx.key[pubKeyLenOffset+2 : pubKeyLenOffset+2+pubKeyLen]

	sig := skx.key[pubKeyLenOffset+2+pubKeyLen:]
	if len(sig) < 2 {
		return errServerKeyExchange
	}

	sigAndHash := signatureAndHash{signature: ka.sigType}
	if ka.version >= VersionTLS12 {
		// handle SignatureAndHashAlgorithm
		sigAndHash = signatureAndHash{hash: sig[0], signature: sig[1]}
		if sigAndHash.signature != ka.sigType {
			return errServerKeyExchange
		}
		sig = sig[2:]
		if len(sig) < 2 {
			return errServerKeyExchange
		}
	}

	sigLen := int(sig[0])<<8 | int(sig[1])
	if sigLen+2 != len(sig) {
		return errServerKeyExchange
	}
	sig = sig[2:]

	lenServerDHParams := 2 + pLen + 2 + gLen + 2 + pubKeyLen
	serverDHParams := make([]byte, lenServerDHParams)   // we know the exact size in advance
	copy(serverDHParams, skx.key[:lenServerDHParams+1]) // everything in the skx up until the sig
	digest, hashFunc, err := hashForServerKeyExchange(sigAndHash, ka.version, clientHello.random, serverHello.random, serverDHParams)
	if err != nil {
		return err
	}

	switch ka.sigType {
	//case signatureECDSA: // Currently there are no ciphersuites implemented in crypto/tls that use DHE and ECDSA
	case signatureRSA:
		pubKey, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return errors.New("tls: DHE RSA requires a RSA server public key")
		}
		if err := rsa.VerifyPKCS1v15(pubKey, hashFunc, digest, sig); err != nil {
			return err
		}
	default:
		return errors.New("tls: unknown DHE signature algorithm")
	}

	// store server's dh params in ka
	ka.p = new(big.Int).SetBytes(serverP)
	ka.g = new(big.Int).SetBytes(serverG)
	ka.Ys = new(big.Int).SetBytes(serverPubKey)

	return nil
}

func (ka *dheKeyAgreement) generateClientKeyExchange(config *Config, clientHello *clientHelloMsg, cert *x509.Certificate) ([]byte, *clientKeyExchangeMsg, error) {
	var preMasterSecret []byte

	pMinus1 := new(big.Int).Sub(ka.p, bigOne)

	// create a private key based on server's p and g
	var x *big.Int
	for {
		var err error
		if x, err = rand.Int(config.rand(), pMinus1); err != nil {
			return nil, nil, err
		}
		if x.Sign() > 0 {
			break
		}
	}

	// create a public key and immediately get the bytes, since that's all we'll need
	XBytes := new(big.Int).Exp(ka.g, x, ka.p).Bytes()
	lenXBytes := len(XBytes)

	// derive Z
	// RFC 5346 8.1.2 The negotiated key (Z) is used as the pre_master_secret. Leading bytes of Z that
	// contain all zero bits are stripped before it is used as the pre_master_secret.
	if ka.Ys.Cmp(bigOne) <= 0 || ka.Ys.Cmp(pMinus1) >= 0 {
		return nil, nil, errors.New("tls: Server DH parameter out of bounds")
	}
	preMasterSecret = new(big.Int).Exp(ka.Ys, x, ka.p).Bytes()

	ckx := new(clientKeyExchangeMsg)
	ckx.ciphertext = make([]byte, 2+lenXBytes)
	ckx.ciphertext[0] = byte(lenXBytes >> 8)
	ckx.ciphertext[1] = byte(lenXBytes)
	copy(ckx.ciphertext[2:], XBytes)

	return preMasterSecret, ckx, nil
}
