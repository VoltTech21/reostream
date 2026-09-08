package baichuan

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"fmt"
	"strings"
)

// xmlKey is the BC cipher key. It is the same on every camera.
var xmlKey = [8]byte{0x1f, 0x2d, 0x3c, 0x4b, 0x5a, 0x69, 0x78, 0xff}

// BCCrypt applies the BC cipher. It is XOR, so the same call encrypts and
// decrypts. The key is rotated by the header's encryption offset and every
// byte is additionally XORed with the low byte of that offset.
//
// This cipher covers the handshake only: the negotiation reply, the login
// request and the DeviceInfo reply. Everything after login is AES.
func BCCrypt(offset uint32, buf []byte) []byte {
	out := make([]byte, len(buf))
	off := int(offset % 8)
	ob := byte(offset)
	for i, c := range buf {
		out[i] = c ^ xmlKey[(off+i)%8] ^ ob
	}
	return out
}

// aesIV is fixed in the camera firmware.
var aesIV = []byte("0123456789abcdef")

// AESKey derives the AES-128 key from the login nonce and the plaintext
// password: the uppercase hex MD5 of "nonce-password", truncated to 16 chars.
func AESKey(nonce, password string) []byte {
	sum := md5.Sum([]byte(nonce + "-" + password))
	return []byte(strings.ToUpper(fmt.Sprintf("%x", sum))[:16])
}

// AESEncrypt applies AES-128-CFB with the fixed IV.
func AESEncrypt(key, buf []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("baichuan: aes: %w", err)
	}
	out := make([]byte, len(buf))
	cipher.NewCFBEncrypter(block, aesIV).XORKeyStream(out, buf)
	return out, nil
}

// AESDecrypt reverses AESEncrypt.
func AESDecrypt(key, buf []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("baichuan: aes: %w", err)
	}
	out := make([]byte, len(buf))
	cipher.NewCFBDecrypter(block, aesIV).XORKeyStream(out, buf)
	return out, nil
}
