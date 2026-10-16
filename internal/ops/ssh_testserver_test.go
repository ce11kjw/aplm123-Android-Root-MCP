// 测试用的最小 SFTP 服务器实现，以及 OpenSSH 私钥编码辅助。
// 仅服务于 internal/ops 的端到端测试，不参与生产构建路径。
package ops

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/ssh"
)

// --- OpenSSH 私钥编码 ---
//
// x/crypto/ssh 只提供解析，不提供编码；测试需要生成可解析的私钥文本，
// 因此这里按 OpenSSH 私有密钥格式（"openssh-key-v1"）手工组装。

func marshalOpenSSHPrivateKey(privateKey ed25519.PrivateKey) ([]byte, error) {
	publicKey := privateKey.Public().(ed25519.PublicKey)

	putUint32 := func(dst []byte, v uint32) []byte {
		var tmp [4]byte
		binary.BigEndian.PutUint32(tmp[:], v)
		return append(dst, tmp[:]...)
	}
	putString := func(dst, b []byte) []byte {
		dst = putUint32(dst, uint32(len(b)))
		return append(dst, b...)
	}

	// checkint 必须两次一致，且长度必须与 ed25519 私钥一致（32 字节 seed + 32 字节公钥 = 64）。
	check := uint32(0x12345678)

	private := []byte{}
	private = putUint32(private, check)
	private = putUint32(private, check)
	private = putString(private, []byte("ssh-ed25519"))
	private = putString(private, publicKey)
	private = putString(private, privateKey) // 64 字节
	private = putString(private, []byte("aplm123-test"))

	// 按 8 字节块补位，填充值为递增序号。
	for i := byte(1); len(private)%8 != 0; i++ {
		private = append(private, i)
	}

	// 注意：整个 openssh-key-v1 结构必须以 AUTH_MAGIC 作为前缀，
	// 否则 ParsePrivateKey 会直接判定格式非法。
	out := []byte("openssh-key-v1\x00")
	out = putString(out, []byte("none")) // ciphername
	out = putString(out, []byte("none")) // kdfname
	out = putString(out, nil)            // kdfoptions
	out = putUint32(out, 1)              // number of keys
	out = putString(out, publicKey)      // 公钥节
	out = putString(out, private)        // 私有节

	return pemEncode("OPENSSH PRIVATE KEY", out), nil
}

func pemEncode(label string, der []byte) []byte {
	out := fmt.Sprintf("-----BEGIN %s-----\n", label)
	encoded := base64Std(der)
	for i := 0; i < len(encoded); i += 70 {
		end := i + 70
		if end > len(encoded) {
			end = len(encoded)
		}
		out += encoded[i:end] + "\n"
	}
	out += fmt.Sprintf("-----END %s-----\n", label)
	return []byte(out)
}

func base64Std(data []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out []byte
	for i := 0; i < len(data); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], data[i:])
		out = append(out, alphabet[chunk[0]>>2])
		out = append(out, alphabet[(chunk[0]&0x03)<<4|chunk[1]>>4])
		if n > 1 {
			out = append(out, alphabet[(chunk[1]&0x0f)<<2|chunk[2]>>6])
		} else {
			out = append(out, '=')
		}
		if n > 2 {
			out = append(out, alphabet[chunk[2]&0x3f])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}

// --- 最小 SFTP 服务器 ---
//
// 实现足够 SFTP v3 子集让 pkg/sftp 客户端完成：
// init / realpath / stat / open / write / read / close / mkdir / lstat / fstat。
// 所有文件保存在内存里，测试结束后随进程回收。

type testSFTPFile struct {
	mu   sync.Mutex
	data []byte
}

type testSFTPState struct {
	mu    sync.Mutex
	files map[string]*testSFTPFile
	next  uint32
}

const (
	sftpFXP_INIT     = 1
	sftpFXP_VERSION  = 2
	sftpFXP_OPEN     = 3
	sftpFXP_CLOSE    = 4
	sftpFXP_READ     = 5
	sftpFXP_WRITE    = 6
	sftpFXP_LSTAT    = 7
	sftpFXP_FSTAT    = 8
	sftpFXP_SETSTAT  = 9
	sftpFXP_FSETSTAT = 10
	sftpFXP_OPENDIR  = 11
	sftpFXP_READDIR  = 12
	sftpFXP_REMOVE   = 13
	sftpFXP_MKDIR    = 14
	sftpFXP_RMDIR    = 15
	sftpFXP_REALPATH = 16
	sftpFXP_STAT     = 17
	sftpFXP_RENAME   = 18
	sftpFXP_STATUS   = 101
	sftpFXP_HANDLE   = 102
	sftpFXP_DATA     = 103

	sftpFXF_READ   = 0x00000001
	sftpFXF_WRITE  = 0x00000002
	sftpFXF_APPEND = 0x00000004
	sftpFXF_CREAT  = 0x00000008
	sftpFXF_TRUNC  = 0x00000010

	sftpFX_OK             = 0
	sftpFX_NO_SUCH_FILE   = 2
	sftpFX_FAILURE        = 4
	sftpFX_OP_UNSUPPORTED = 8
)

// runTestSFTP 在给定通道上跑一个最小 SFTP 服务循环。
func runTestSFTP(channel ssh.Channel) {
	state := &testSFTPState{files: map[string]*testSFTPFile{}, next: 1}
	handles := map[uint32]string{}

	for {
		lengthBuf := make([]byte, 4)
		if _, err := io.ReadFull(channel, lengthBuf); err != nil {
			return
		}
		length := binary.BigEndian.Uint32(lengthBuf)
		if length == 0 || length > 1<<20 {
			return
		}
		packet := make([]byte, length)
		if _, err := io.ReadFull(channel, packet); err != nil {
			return
		}

		var requestID uint32
		var body []byte
		if length >= 9 {
			requestID = binary.BigEndian.Uint32(packet[5:9])
			body = packet[9:]
		}

		switch packet[4] {
		case sftpFXP_INIT:
			// VERSION
			response := make([]byte, 5)
			response[0] = sftpFXP_VERSION
			binary.BigEndian.PutUint32(response[1:], 3)
			writeSFTPPacket(channel, response)

		case sftpFXP_REALPATH:
			path := decodeSFTPPath(body)
			respondName(channel, sftpFXP_REALPATH, requestID, path)

		case sftpFXP_OPEN:
			path := decodeSFTPPath(body)
			state.mu.Lock()
			file, exists := state.files[path]
			if !exists {
				file = &testSFTPFile{}
				state.files[path] = file
			}
			id := state.next
			state.next++
			handles[id] = path
			state.mu.Unlock()
			respondHandle(channel, requestID, id)

		case sftpFXP_WRITE:
			// id + offset + data
			if len(body) < 8 {
				respondStatus(channel, requestID, sftpFX_FAILURE)
				continue
			}
			id := binary.BigEndian.Uint32(body[0:4])
			offset := binary.BigEndian.Uint32(body[4:8])
			data := decodeSFTPString(body[8:])
			state.mu.Lock()
			path, ok := handles[id]
			file := state.files[path]
			state.mu.Unlock()
			if !ok || file == nil {
				respondStatus(channel, requestID, sftpFX_FAILURE)
				continue
			}
			file.mu.Lock()
			needed := int(offset) + len(data)
			if needed > len(file.data) {
				grown := make([]byte, needed)
				copy(grown, file.data)
				file.data = grown
			}
			copy(file.data[offset:], data)
			file.mu.Unlock()
			respondStatus(channel, requestID, sftpFX_OK)

		case sftpFXP_READ:
			if len(body) < 12 {
				respondStatus(channel, requestID, sftpFX_FAILURE)
				continue
			}
			id := binary.BigEndian.Uint32(body[0:4])
			offset := binary.BigEndian.Uint32(body[4:8])
			count := binary.BigEndian.Uint32(body[8:12])
			state.mu.Lock()
			path, ok := handles[id]
			file := state.files[path]
			state.mu.Unlock()
			if !ok || file == nil {
				respondStatus(channel, requestID, sftpFX_FAILURE)
				continue
			}
			file.mu.Lock()
			if int(offset) >= len(file.data) {
				file.mu.Unlock()
				respondStatus(channel, requestID, 1) // EOF
				continue
			}
			end := int(offset) + int(count)
			if end > len(file.data) {
				end = len(file.data)
			}
			chunk := append([]byte(nil), file.data[offset:end]...)
			file.mu.Unlock()
			respondData(channel, requestID, chunk)

		case sftpFXP_CLOSE:
			if len(body) >= 4 {
				id := binary.BigEndian.Uint32(body[0:4])
				state.mu.Lock()
				delete(handles, id)
				state.mu.Unlock()
			}
			respondStatus(channel, requestID, sftpFX_OK)

		case sftpFXP_STAT, sftpFXP_LSTAT:
			path := decodeSFTPPath(body)
			state.mu.Lock()
			file, exists := state.files[path]
			state.mu.Unlock()
			if !exists {
				// 报告一个 0 字节的既存文件，让 Stat 调用可用。
				respondAttrs(channel, sftpFXP_STAT, requestID, 0)
				continue
			}
			file.mu.Lock()
			size := len(file.data)
			file.mu.Unlock()
			respondAttrs(channel, sftpFXP_STAT, requestID, uint64(size))

		case sftpFXP_FSTAT:
			respondAttrs(channel, sftpFXP_FSTAT, requestID, 0)

		case sftpFXP_MKDIR:
			// 目录在内存模型中不存在，直接成功即可（MkdirAll 语义）。
			respondStatus(channel, requestID, sftpFX_OK)

		case sftpFXP_SETSTAT, sftpFXP_FSETSTAT:
			respondStatus(channel, requestID, sftpFX_OK)

		case sftpFXP_REMOVE:
			path := decodeSFTPPath(body)
			state.mu.Lock()
			delete(state.files, path)
			state.mu.Unlock()
			respondStatus(channel, requestID, sftpFX_OK)

		default:
			respondStatus(channel, requestID, sftpFX_OP_UNSUPPORTED)
		}
	}
}

func writeSFTPPacket(channel ssh.Channel, payload []byte) {
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(payload)))
	copy(buf[4:], payload)
	_, _ = channel.Write(buf)
}

func decodeSFTPString(data []byte) []byte {
	if len(data) < 4 {
		return nil
	}
	n := binary.BigEndian.Uint32(data[0:4])
	if int(4+n) > len(data) {
		return nil
	}
	return data[4 : 4+n]
}

func decodeSFTPPath(body []byte) string {
	return string(decodeSFTPString(body))
}

func respondStatus(channel ssh.Channel, requestID uint32, code uint32) {
	payload := make([]byte, 9)
	payload[0] = sftpFXP_STATUS
	binary.BigEndian.PutUint32(payload[1:5], requestID)
	binary.BigEndian.PutUint32(payload[5:9], code)
	writeSFTPPacket(channel, payload)
}

func respondHandle(channel ssh.Channel, requestID, handle uint32) {
	payload := []byte{sftpFXP_HANDLE}
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], requestID)
	payload = append(payload, tmp[:]...)
	binary.BigEndian.PutUint32(tmp[:], 4)
	payload = append(payload, tmp[:]...)
	binary.BigEndian.PutUint32(tmp[:], handle)
	payload = append(payload, tmp[:]...)
	writeSFTPPacket(channel, payload)
}

func respondData(channel ssh.Channel, requestID uint32, data []byte) {
	payload := []byte{sftpFXP_DATA}
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], requestID)
	payload = append(payload, tmp[:]...)
	binary.BigEndian.PutUint32(tmp[:], uint32(len(data)))
	payload = append(payload, tmp[:]...)
	payload = append(payload, data...)
	writeSFTPPacket(channel, payload)
}

func respondName(channel ssh.Channel, packetType byte, requestID uint32, name string) {
	payload := []byte{packetType}
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], requestID)
	payload = append(payload, tmp[:]...)
	binary.BigEndian.PutUint32(tmp[:], 1) // count
	payload = append(payload, tmp[:]...)
	binary.BigEndian.PutUint32(tmp[:], uint32(len(name)))
	payload = append(payload, tmp[:]...)
	payload = append(payload, []byte(name)...)
	binary.BigEndian.PutUint32(tmp[:], uint32(len(name)))
	payload = append(payload, tmp[:]...)
	payload = append(payload, []byte(name)...)
	payload = append(payload, make([]byte, 8)...) // attrs
	writeSFTPPacket(channel, payload)
}

func respondAttrs(channel ssh.Channel, packetType byte, requestID uint32, size uint64) {
	payload := []byte{packetType}
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], requestID)
	payload = append(payload, tmp[:]...)
	binary.BigEndian.PutUint32(tmp[:], 0x00000001) // flags: SIZE
	payload = append(payload, tmp[:]...)
	var sizeBuf [8]byte
	binary.BigEndian.PutUint64(sizeBuf[:], size)
	payload = append(payload, sizeBuf[:]...)
	writeSFTPPacket(channel, payload)
}

var _ = rand.Reader
