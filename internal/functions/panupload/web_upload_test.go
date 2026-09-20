package panupload

import (
	"bytes"
	"crypto/aes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tickstep/cloudpan189-api/cloudpan"
	"github.com/tickstep/cloudpan189-go/internal/localfile"
)

type uploadTestTransport func(*http.Request) (*http.Response, error)

func (f uploadTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func uploadTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestWebUploadOfficialAuthAndSignature(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	w := newWebUploader(cloudpan.WebLoginToken{CookieLoginUser: "test-cookie"})
	calls := 0
	w.client.Transport = uploadTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		c, err := r.Cookie("COOKIE_LOGIN_USER")
		if err != nil || c.Value != "test-cookie" {
			t.Fatal("认证请求缺少 Cookie")
		}
		if r.URL.RawQuery != "" {
			t.Fatal("认证信息不应出现在 URL")
		}
		switch calls {
		case 1:
			if r.URL.Path != "/api/portal/v2/getUserBriefInfo.action" {
				t.Fatal(r.URL.Path)
			}
			return uploadTestResponse(200, `{"res_code":0,"sessionKey":"web-session"}`), nil
		case 2:
			if r.URL.Path != "/api/security/generateRsaKey.action" {
				t.Fatal(r.URL.Path)
			}
			b, _ := json.Marshal(map[string]string{"pubKey": base64.StdEncoding.EncodeToString(der), "pkId": "test-key"})
			return uploadTestResponse(200, string(b)), nil
		default:
			t.Fatal("重复获取认证信息")
			return nil, nil
		}
	})
	params := map[string]string{"fileName": url.QueryEscape("中文 &+.rar"), "fileSize": "247859701", "sliceSize": "10485760", "lazyCheck": "1"}
	req, err := w.newWebRequest("/person/initMultiUpload", params)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := base64.StdEncoding.DecodeString(req.Header.Get("EncryptionText"))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := rsa.DecryptPKCS1v15(rand.Reader, key, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(secret[:16])
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := hex.DecodeString(req.URL.Query().Get("params"))
	if err != nil {
		t.Fatal(err)
	}
	plain := make([]byte, len(ciphertext))
	for i := 0; i < len(plain); i += 16 {
		block.Decrypt(plain[i:i+16], ciphertext[i:i+16])
	}
	padding := int(plain[len(plain)-1])
	plain = plain[:len(plain)-padding]
	expected := "fileName=" + params["fileName"] + "&fileSize=247859701&lazyCheck=1&sliceSize=10485760"
	if string(plain) != expected {
		t.Fatalf("参数解密不一致: %s", plain)
	}
	mac := hmac.New(sha1.New, secret)
	fmt.Fprintf(mac, "SessionKey=web-session&Operate=GET&RequestURI=/person/initMultiUpload&Date=%s&params=%s", req.Header.Get("X-Request-Date"), req.URL.Query().Get("params"))
	if req.Header.Get("Signature") != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("签名不匹配")
	}
	if req.Header.Get("PkId") != "test-key" || req.Header.Get("SessionKey") != "web-session" || req.Header.Get("Date") != "" {
		t.Fatal("网页认证头不正确")
	}
	if req.Header.Get("Cookie") != "" || len(req.URL.Query()) != 1 {
		t.Fatal("上传请求携带了多余认证或查询参数")
	}
	next, err := w.newWebRequest("/person/getMultiUploadUrls", map[string]string{"uploadFileId": "id"})
	if err != nil {
		t.Fatal(err)
	}
	encrypted2, _ := base64.StdEncoding.DecodeString(next.Header.Get("EncryptionText"))
	secret2, err := rsa.DecryptPKCS1v15(rand.Reader, key, encrypted2)
	if err != nil || bytes.Equal(secret, secret2) {
		t.Fatal("临时密钥未更新")
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestWebUploadErrorResponses(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"服务器错误", 500, `{"error":"Internal Server Error","status":500}`, "HTTP 500: Internal Server Error"},
		{"业务错误", 200, `{"code":"InvalidSessionKey","msg":"会话失效"}`, "HTTP 200: InvalidSessionKey: 会话失效"},
		{"字符串成功码", 200, `{"res_code":"0"}`, ""},
		{"数字错误码", 200, `{"res_code":-1,"res_message":"失败"}`, "HTTP 200: -1: 失败"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &webUploader{publicKey: &key.PublicKey, pkID: "test", sessionKey: "test", client: &http.Client{Transport: uploadTestTransport(func(*http.Request) (*http.Response, error) { return uploadTestResponse(tc.status, tc.body), nil })}}
			err := w.webRequest("/person/initMultiUpload", map[string]string{"fileName": "test"}, nil)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v want %s", err, tc.want)
			}
		})
	}
}

func TestWebUploadRequiresCookie(t *testing.T) {
	w := newWebUploader(cloudpan.WebLoginToken{})
	if _, err := w.newWebRequest("/person/initMultiUpload", nil); err == nil {
		t.Fatal("缺少 Cookie 应返回错误")
	}
}

func TestLargeFileIgnoresPCResumeState(t *testing.T) {
	task := &UploadTaskUnit{SavePath: "/目标/file.rar", LocalFileChecksum: &localfile.LocalFileEntity{LocalFileMeta: localfile.LocalFileMeta{Length: maxSingleUploadSize + 1, UploadFileId: "stale-pc-task"}}}
	// 不提供 PanClient 和旧数据库；大文件不应访问 PC 续传接口。
	task.prepareFile()
	if task.Step != StepUploadPrepareUpload || task.panFile != "file.rar" {
		t.Fatal("未进入分片准备阶段")
	}
}

func TestPresignedPartURLUnchanged(t *testing.T) {
	original := "https://example.invalid/part?signature=a%2Bb&partNumber=1"
	w := &webUploader{client: &http.Client{Transport: uploadTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != original || r.Method != "PUT" {
			t.Fatal("预签名地址被修改")
		}
		if r.Header.Get("Content-MD5") != "a+b==" {
			t.Fatal("校验头被修改")
		}
		if r.Header.Get("SessionKey") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("分片携带了多余认证")
		}
		return uploadTestResponse(200, ""), nil
	})}}
	if err := w.putSlice(original, "Content-MD5=a+b==", []byte("data")); err != nil {
		t.Fatal(err)
	}
}
