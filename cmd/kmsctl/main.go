// Command kmsctl 是 kms-manage 的运维 CLI：
//
//	kmsctl init-key <dir> <id>              # 生成一个新 master key 并设为 active
//	kmsctl add-key  <dir> <id>              # 追加一个 key（不改 active）
//	kmsctl set-active <dir> <id>            # 改 ACTIVE 指向
//	kmsctl list <dir>                       # 打印当前 keystore 概况
//	kmsctl encrypt <addr> <context> <text>  # 远程 Encrypt（走 gRPC）
//	kmsctl decrypt <addr> <context> <ct>    # 远程 Decrypt
//
// 前四个是纯本地（直接改 keystore 目录），后两个连 gRPC。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	kmsv1 "github.com/xiongwp/kms-manage/api/proto/kms/v1"
	"github.com/xiongwp/kms-manage/internal/keystore"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "init-key":
		mustArgs(4)
		must(initKey(os.Args[2], os.Args[3]))
	case "add-key":
		mustArgs(4)
		must(addKey(os.Args[2], os.Args[3]))
	case "set-active":
		mustArgs(4)
		must(setActive(os.Args[2], os.Args[3]))
	case "list":
		mustArgs(3)
		must(listKeys(os.Args[2]))
	case "encrypt":
		mustArgs(5)
		must(remoteEncrypt(os.Args[2], os.Args[3], os.Args[4]))
	case "decrypt":
		mustArgs(5)
		must(remoteDecrypt(os.Args[2], os.Args[3], os.Args[4]))
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `kmsctl subcommands:
  init-key <dir> <id>                   generate master key + set ACTIVE
  add-key  <dir> <id>                   append another master key
  set-active <dir> <id>                 change ACTIVE
  list <dir>                            show keys + active
  encrypt <addr> <context> <plaintext>  call gRPC Encrypt
  decrypt <addr> <context> <ciphertext> call gRPC Decrypt`)
}

// ─── local keystore ops ────────────────────────────────────────

func initKey(dir, id string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeRandomKey(dir, id); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ACTIVE"), []byte(id+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s/%s.key + ACTIVE=%s\n", dir, id, id)
	return nil
}

func addKey(dir, id string) error {
	if err := writeRandomKey(dir, id); err != nil {
		return err
	}
	fmt.Printf("wrote %s/%s.key (ACTIVE unchanged)\n", dir, id)
	return nil
}

func setActive(dir, id string) error {
	if _, err := os.Stat(filepath.Join(dir, id+".key")); err != nil {
		return fmt.Errorf("no such key: %s", id)
	}
	if err := os.WriteFile(filepath.Join(dir, "ACTIVE"), []byte(id+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Printf("ACTIVE → %s\n", id)
	return nil
}

func listKeys(dir string) error {
	s, err := keystore.Load(dir)
	if err != nil {
		return err
	}
	fmt.Printf("active: %s\n", s.ActiveKeyID())
	fmt.Println("keys:")
	for _, m := range s.List() {
		tag := ""
		if m.ID == s.ActiveKeyID() {
			tag = " [ACTIVE]"
		}
		fmt.Printf("  - %s  %s  %s%s\n", m.ID, m.Algorithm, m.CreatedAt.Format("2006-01-02"), tag)
	}
	return nil
}

func writeRandomKey(dir, id string) error {
	buf := make([]byte, 32) // 256-bit
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	path := filepath.Join(dir, id+".key")
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("refuse to overwrite existing %s", path)
	}
	return os.WriteFile(path, []byte(hex.EncodeToString(buf)+"\n"), 0o600)
}

// ─── remote gRPC ops ──────────────────────────────────────────

func remoteEncrypt(addr, contextStr, plaintext string) error {
	cli, cleanup, err := dial(addr)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := cli.Encrypt(ctx, &kmsv1.EncryptRequest{
		Plaintext: []byte(plaintext),
		Context:   contextStr,
	})
	if err != nil {
		return err
	}
	fmt.Println(out.GetCiphertext())
	return nil
}

func remoteDecrypt(addr, contextStr, ciphertext string) error {
	cli, cleanup, err := dial(addr)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := cli.Decrypt(ctx, &kmsv1.DecryptRequest{
		Ciphertext: ciphertext,
		Context:    contextStr,
	})
	if err != nil {
		return err
	}
	fmt.Println(string(out.GetPlaintext()))
	return nil
}

func dial(addr string) (kmsv1.KMSServiceClient, func(), error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return kmsv1.NewKMSServiceClient(conn), func() { _ = conn.Close() }, nil
}

// ─── tiny utils ────────────────────────────────────────────────

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
func mustArgs(n int) {
	if len(os.Args) < n {
		usage()
		os.Exit(1)
	}
}
