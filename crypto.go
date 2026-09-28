package govoucherpool

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
)

// digestSize 是 SHA-256 摘要长度。
const digestSize = sha256.Size

// newSalt 生成一个密码学随机的批次盐。
func newSalt() ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return salt, nil
}

// newPepper 生成 Store 级别的随机胡椒，进一步降低摘要被离线爆破的风险。
func newPepper() ([]byte, error) {
	p := make([]byte, 32)
	if _, err := rand.Read(p); err != nil {
		return nil, err
	}
	return p, nil
}

// digestCode 计算券码的安全摘要：
//
//	SHA-256(pepper || salt || code)
//
// 明文不写内存对象、不入索引、不出现在任何错误信息中。
func digestCode(pepper, salt []byte, code string) []byte {
	h := sha256.New()
	h.Write(pepper)
	h.Write(salt)
	h.Write([]byte(code))
	return h.Sum(nil)
}

// sameDigest 以恒定时间比较两个摘要，避免时序侧信道。
func sameDigest(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// newID 生成带前缀的随机 ID（128bit 随机数，hex 编码）。
func newID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

// tokenSafe 将随机字节编码为 URL 安全文本，方便调用方直接生成券码。
func tokenSafe(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// GenerateCode 返回一个密码学随机、URL 安全的券码明文。
// 调用方负责在登记（RegisterVouchers）后丢弃它；服务端只保存摘要。
func GenerateCode() (string, error) {
	b := make([]byte, 24) // 192bit，Base64URL 后约 32 字符
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return tokenSafe(b), nil
}
