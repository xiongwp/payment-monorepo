package secret

import (
	"context"
	"strings"
	"testing"

	kmsv1 "github.com/xiongwp/kms-manage/api/proto/kms/v1"
	kmsth "github.com/xiongwp/kms-manage/testhelper"

	"github.com/xiongwp/payment-core/internal/kmsclient"
)

// bufClient 把 kms-manage testhelper 起的 gRPC client 包成 payment-core 的
// kmsclient.Client 接口。实际生产走 kmsclient.Dial，测试里图省事直接走 bufconn。
type bufClient struct{ c kmsv1.KMSServiceClient }

func (b bufClient) Encrypt(ctx context.Context, plaintext []byte, aadContext string) (string, error) {
	r, err := b.c.Encrypt(ctx, &kmsv1.EncryptRequest{Plaintext: plaintext, Context: aadContext})
	if err != nil {
		return "", err
	}
	return r.GetCiphertext(), nil
}
func (b bufClient) Decrypt(ctx context.Context, ct, aadContext string) ([]byte, error) {
	r, err := b.c.Decrypt(ctx, &kmsv1.DecryptRequest{Ciphertext: ct, Context: aadContext})
	if err != nil {
		return nil, err
	}
	return r.GetPlaintext(), nil
}
func (b bufClient) Close() error { return nil }

var _ kmsclient.Client = bufClient{}

func TestResolve_PassthroughOnPlain(t *testing.T) {
	got, err := Resolve(context.Background(), nil, "svc:x", "plain-value")
	if err != nil {
		t.Fatal(err)
	}
	if got != "plain-value" {
		t.Fatal(got)
	}
}

func TestResolve_FailsOnCiphertextWithoutClient(t *testing.T) {
	_, err := Resolve(context.Background(), nil, "svc:x", "kms:v1:main:QUFBQUFBQUFBQUFBQUFBQQ")
	if err == nil || !strings.Contains(err.Error(), "no kms client") {
		t.Fatalf("want 'no kms client' err, got %v", err)
	}
}

func TestResolve_DecryptsThroughKMS(t *testing.T) {
	kms := kmsth.Start(t, kmsth.DefaultConfig())
	cli := bufClient{c: kms.Client}

	enc, err := kms.Client.Encrypt(context.Background(), &kmsv1.EncryptRequest{
		Plaintext: []byte("s3cret-token"),
		Context:   "svc:paycore:auth_tokens",
	})
	if err != nil {
		t.Fatal(err)
	}

	plain, err := Resolve(context.Background(), cli, "svc:paycore:auth_tokens", enc.GetCiphertext())
	if err != nil {
		t.Fatal(err)
	}
	if plain != "s3cret-token" {
		t.Fatalf("got %q", plain)
	}
}

func TestResolve_FailsOnWrongContext(t *testing.T) {
	kms := kmsth.Start(t, kmsth.DefaultConfig())
	cli := bufClient{c: kms.Client}

	enc, _ := kms.Client.Encrypt(context.Background(), &kmsv1.EncryptRequest{
		Plaintext: []byte("x"), Context: "ctxA",
	})
	if _, err := Resolve(context.Background(), cli, "ctxB", enc.GetCiphertext()); err == nil {
		t.Fatal("different AAD must reject")
	}
}

func TestResolveStringSlice_Mixed(t *testing.T) {
	kms := kmsth.Start(t, kmsth.DefaultConfig())
	cli := bufClient{c: kms.Client}

	enc, _ := kms.Client.Encrypt(context.Background(), &kmsv1.EncryptRequest{
		Plaintext: []byte("secret"), Context: "svc:paycore:auth_tokens",
	})
	out, err := ResolveStringSlice(context.Background(), cli,
		"svc:paycore:auth_tokens",
		[]string{"plaintok", enc.GetCiphertext()},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out[0] != "plaintok" || out[1] != "secret" {
		t.Fatalf("got %+v", out)
	}
}

func TestResolveBytes_Decrypts(t *testing.T) {
	kms := kmsth.Start(t, kmsth.DefaultConfig())
	cli := bufClient{c: kms.Client}

	enc, _ := kms.Client.Encrypt(context.Background(), &kmsv1.EncryptRequest{
		Plaintext: []byte{0x00, 0x01, 0x02, 0x03}, Context: "svc:paycore:mtls_key",
	})
	got, err := ResolveBytes(context.Background(), cli, "svc:paycore:mtls_key", enc.GetCiphertext())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[3] != 0x03 {
		t.Fatalf("got %v", got)
	}
}

func TestResolveBytes_Passthrough(t *testing.T) {
	got, err := ResolveBytes(context.Background(), nil, "x", "plain-bytes")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "plain-bytes" {
		t.Fatalf("%q", got)
	}
}
