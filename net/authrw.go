package net

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/sahib/brig/catfs/mio/compress"
	"github.com/sahib/brig/util"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/sha3"
)

const (
	// nonceSize is the size in bytes of the challenge we send to the remote
	nonceSize = 62
	// MaxMessageSize is the max size of a messsage that can be send to us.
	// The limit is arbitrary and should avoid being spammed by huge messages.
	// (Later on we could also implement a proper streaming protocol)
	MaxMessageSize = 16 * 1024 * 1024
)

// PrivDecrypter is anything that can decrypt a message
// that was previously encrypted with a public key.
type PrivDecrypter interface {
	Decrypt(data []byte) ([]byte, error)
}

// RemoteChecker is a function that is called once the public key
// of the remote has been received. If an error is returned,
// the authentication will fail. Use this to check the remote's public key
// against the fingerprint we store of it.
type RemoteChecker func(remotePubKey []byte) error

// AuthReadWriter acts as a layer on top of a normal io.ReadWriteCloser
// that adds authentication of the communication partners.
// It does this by employing the following protocol:
//
// 1) Upon opening the connection, the public keys of both partners
//    are exchanged. The received public key is hashed and checked to
//    be the same as the fingerprint we're storing from this person.
//    (This should suffice as authentication of the remote user)
//
// 2) A random nonce of 62 bytes is generated and encrypted with the
//    remote's public key. The resulting ciphertext is then send to the
//    remote. On their side they decrypt the ciphertext (proving that
//    they possess the respective private key).
//
// 3) The resulting nonce from the remote is then hashed with sha3
//    and send back. Each sides check if the response matched the challenge.
//    If so, the user is authenticated. The nonces are then used to
//    generate a symmetric key (using scrypt) which is then used to encrypt
//    further communication and to authenticate messages.
//
// 4) Further communication writes messages with a hmac, a 4 byte size header
//    and the actual payload.
type AuthReadWriter struct {
	// Raw underlying network connection
	rwc io.ReadWriteCloser

	// The data of our public key
	ownPubKey []byte

	// The name we advertise to the remote
	ownName string

	// The name remote advertised to us
	remoteName string

	// The remote's public key, once received (nil before)
	remotePubKey []byte

	// privKey is capable of decrypting a message send to us.
	privKey PrivDecrypter

	// Checker callback to authenticate remote's public key
	remoteChecker RemoteChecker

	// encrypted read writer
	cryptedRW io.ReadWriter

	// Symmetric key used to encrypt/verify after authentication
	symkey []byte

	// Set to true after the remote was authenticated
	authorised bool

	// buffer to implement io.Reader's streaming properties
	readBuf *bytes.Buffer
}

// NewAuthReadWriter returns a new AuthReadWriter, adding an auth layer on top
// of `rwc`. `privKey` is used to decrypt the remote's challenge, while
// `ownPubKey` is the pub key we send to them. `remoteChecker` is a callback
// that is being used by the user to verify if the remote's public key
// is the one we're expecting.
func NewAuthReadWriter(
	rwc io.ReadWriteCloser,
	privKey PrivDecrypter,
	ownPubKey []byte,
	ownName string,
	remoteChecker RemoteChecker,
) *AuthReadWriter {
	return &AuthReadWriter{
		rwc:           rwc,
		privKey:       privKey,
		ownPubKey:     ownPubKey,
		ownName:       ownName,
		readBuf:       &bytes.Buffer{},
		remoteChecker: remoteChecker,
	}
}

// IsAuthorised will return true if the partner was successfully authenticated.
// It will return false if no call to Read() or Write() was made.
func (ath *AuthReadWriter) IsAuthorised() bool {
	return ath.authorised
}

// writeSizePack prefixes a datablock by it's binary size
func writeSizePack(w io.Writer, data []byte) (int, error) {
	pack := make([]byte, 8)
	binary.LittleEndian.PutUint64(pack, uint64(len(data)))

	if n, err := w.Write(append(pack, data...)); err != nil {
		return n, err
	}

	return len(pack) + len(data), nil
}

// readSizePack reads a 8 byte size prefix and return the following data block.
// If the block appears too large, it will error out.
func readSizePack(r io.Reader) ([]byte, error) {
	sizeBuf := make([]byte, 8)
	if _, err := io.ReadFull(r, sizeBuf); err != nil {
		return nil, err
	}

	size := binary.LittleEndian.Uint64(sizeBuf)

	// Protect against unreasonable sizes:
	if size > 4096 {
		return nil, fmt.Errorf("Auth package is oversized: %d", size)
	}

	buf := make([]byte, size)
	if _, err := io.ReadAtLeast(r, buf, int(size)); err != nil {
		return nil, err
	}

	return buf, nil
}

// encryptWithPubKey encrypted `data` with the key serialized in `pubKeyData`.
func encryptWithPubKey(data, pubKeyData []byte) ([]byte, error) {
	// Load their pubkey from memory:
	ents, err := openpgp.ReadKeyRing(bytes.NewReader(pubKeyData))
	if err != nil {
		return nil, err
	}

	encBuf := &bytes.Buffer{}
	encW, err := openpgp.Encrypt(encBuf, ents, nil, nil, nil)
	if err != nil {
		return nil, err
	}

	if _, err := encW.Write(data); err != nil {
		return nil, err
	}

	if err := encW.Close(); err != nil {
		return nil, err
	}

	return encBuf.Bytes(), nil
}

// RemotePubKey returns the partner's public key, if it was authorised already.
// Otherwise an error will be returned.
func (ath *AuthReadWriter) RemotePubKey() []byte {
	return ath.remotePubKey
}

// RemoteName returns the remote's screen name.
// Note that this name only serves as indication for display
// and should not be relied on since it can be easily faked.
func (ath *AuthReadWriter) RemoteName() string {
	return ath.remoteName
}

// wrap a normal io.ReadWriter into an AES encrypted tunnel.
func wrapEncryptedRW(iv, key []byte, rw io.ReadWriter) (io.ReadWriter, error) {
	blockCipher, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	streamW := &cipher.StreamWriter{
		S: cipher.NewCFBEncrypter(blockCipher, iv),
		W: rw,
	}

	streamR := &cipher.StreamReader{
		S: cipher.NewCFBDecrypter(blockCipher, iv),
		R: rw,
	}

	return struct {
		io.Reader
		io.Writer
	}{
		Reader: streamR,
		Writer: streamW,
	}, nil
}

// runAuth runs the protocol pointed out above.
func (ath *AuthReadWriter) runAuth() error {
	if _, err := writeSizePack(ath.rwc, []byte(ath.ownName)); err != nil {
		return err
	}

	// Write our own pubkey down the line:
	if _, err := writeSizePack(ath.rwc, ath.ownPubKey); err != nil {
		return err
	}

	// Read the advertised remote name.
	// (malicious partners could fake whatever name here,
	//  but we do not rely on the name)
	remoteName, err := readSizePack(ath.rwc)
	if err != nil {
		return err
	}

	ath.remoteName = string(remoteName)

	// Read their pubkey:
	remotePubKey, err := readSizePack(ath.rwc)
	if err != nil {
		return err
	}

	// Check if the hash of the remote pub key matches the fingerprint we have.
	// This is the single most important assertion, because we will accept any
	// valid keypair otherwise.
	if err := ath.remoteChecker(remotePubKey); err != nil {
		return err
	}

	ath.remotePubKey = remotePubKey

	// Generate our own nonce:
	rA := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, rA); err != nil {
		return err
	}

	// Send our challenge encrypted with remote's public key.
	chlForBob, err := encryptWithPubKey(rA, remotePubKey)
	if err != nil {
		return err
	}

	if _, err := writeSizePack(ath.rwc, chlForBob); err != nil {
		return err
	}

	// Read their challenge (nonce encrypted with our pubkey)
	chlFromBob, err := readSizePack(ath.rwc)
	if err != nil {
		return err
	}

	// nonceFromBob is their nonce:
	nonceFromBob, err := ath.privKey.Decrypt(chlFromBob)
	if err != nil {
		return err
	}

	if len(nonceFromBob) != nonceSize {
		return fmt.Errorf(
			"Bad nonce size from partner: %d (not %d)",
			len(nonceFromBob),
			nonceSize,
		)
	}

	// Send back our challenge-response
	respHash := sha3.Sum512(nonceFromBob)
	if _, err := ath.rwc.Write(respHash[:]); err != nil {
		return err
	}

	// Read the response from bob to our challenge
	hashFromBob := make([]byte, 512/8)
	if _, err := io.ReadFull(ath.rwc, hashFromBob); err != nil {
		return err
	}

	ownHash := sha3.Sum512(rA)
	if !bytes.Equal(hashFromBob, ownHash[:]) {
		return fmt.Errorf("Bad nonce; might communicate with imposter")
	}

	keySource := make([]byte, nonceSize)
	for i := range keySource {
		keySource[i] = nonceFromBob[i] ^ rA[i]
	}

	key := util.DeriveKey(keySource, keySource[:nonceSize/2], 32)
	inv := util.DeriveKey(keySource, keySource[nonceSize/2:], aes.BlockSize)

	rw, err := wrapEncryptedRW(inv, key, ath.rwc)
	if err != nil {
		return err
	}

	ath.symkey = key
	ath.cryptedRW = rw
	ath.authorised = true
	return nil
}

// Trigger the authentication machinery manually.
func (ath *AuthReadWriter) Trigger() error {
	if !ath.IsAuthorised() {
		if err := ath.runAuth(); err != nil {
			ath.rwc.Close()
			return err
		}
	}

	return nil
}

// readMessage reads a single message pack from the network.
func (ath *AuthReadWriter) readMessage() ([]byte, error) {
	header := make([]byte, 28+4)

	if _, err := io.ReadFull(ath.rwc, header); err != nil {
		return nil, err
	}

	size := binary.LittleEndian.Uint32(header[28:])
	if size > MaxMessageSize {
		return nil, fmt.Errorf("Message too large (%d/%d)", size, MaxMessageSize)
	}

	buf := make([]byte, size)
	if _, err := io.ReadAtLeast(ath.cryptedRW, buf, int(size)); err != nil {
		return nil, err
	}

	macWriter := hmac.New(sha3.New224, ath.symkey)
	if _, err := macWriter.Write(buf); err != nil {
		return nil, err
	}

	mac := macWriter.Sum(nil)
	if !hmac.Equal(mac, header[:28]) {
		return nil, fmt.Errorf("Mac differs in received metadata message")
	}

	return compress.Unpack(buf)
}

// Read will try to fill `buf` with as many bytes as available.
func (ath *AuthReadWriter) Read(buf []byte) (int, error) {
	if err := ath.Trigger(); err != nil {
		return 0, err
	}

	n := 0
	bufLen := len(buf)

	// Read messages as long `buf` is not full yet.
	for {
		if ath.readBuf.Len() > 0 {
			bn, berr := ath.readBuf.Read(buf)
			if berr != nil && berr != io.EOF {
				return n, berr
			}

			n += bn
			buf = buf[bn:]

			if berr == io.EOF {
				break
			}
		}

		if n >= bufLen {
			return n, nil
		}

		msg, err := ath.readMessage()
		if err != nil {
			return n, err
		}

		if _, werr := ath.readBuf.Write(msg); werr != nil {
			return n, err
		}
	}

	return n, nil
}

// Write conforming to the io.Writer interface
func (ath *AuthReadWriter) Write(buf []byte) (int, error) {
	if err := ath.Trigger(); err != nil {
		return 0, err
	}

	zipBuf, err := compress.Pack(buf, compress.AlgoSnappy)
	if err != nil {
		return -1, err
	}

	macWriter := hmac.New(sha3.New224, ath.symkey)
	if _, err := macWriter.Write(zipBuf); err != nil {
		return -2, err
	}

	mac := macWriter.Sum(nil)
	n, err := ath.rwc.Write(mac)
	if err != nil {
		return -3, err
	}

	if n != len(mac) {
		return -4, fmt.Errorf(
			"Unable to write full mac. Should be %d; was %d",
			len(mac),
			n,
		)
	}

	// Note: this assumes that `cryptedRW` does not pad the data.
	sizeBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(sizeBuf, uint32(len(zipBuf)))

	n, err = ath.rwc.Write(sizeBuf)
	if err != nil {
		return -5, err
	}

	if n != len(sizeBuf) {
		return -6, fmt.Errorf(
			"Unable to write full size buf. Should be %d; was %d",
			len(sizeBuf),
			n,
		)
	}

	return ath.cryptedRW.Write(zipBuf)
}
