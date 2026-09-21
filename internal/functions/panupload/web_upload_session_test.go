package panupload

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestInitRecoversSessionOnce(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	for _, failAgain := range []bool{false, true} {
		t.Run(map[bool]string{false: "恢复成功", true: "恢复后仍失败"}[failAgain], func(t *testing.T) {
			inits, refreshes := 0, 0
			w := &webUploader{sessionKey: "old-session", cookieLoginUser: "old-cookie", accessToken: "access-secret", publicKey: &key.PublicKey, pkID: "old-key"}
			w.client = &http.Client{Transport: uploadTestTransport(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/person/initMultiUpload":
					inits++
					if inits == 1 || failAgain {
						return uploadTestResponse(500, `{"error":"Internal Server Error"}`), nil
					}
					if r.Header.Get("SessionKey") != "fresh-web-session" || r.Header.Get("PkId") != "fresh-key" {
						t.Fatal("未使用新会话和公钥")
					}
					return uploadTestResponse(200, `{"code":"SUCCESS","data":{"uploadFileId":"id"}}`), nil
				case "/getSessionForPC.action":
					refreshes++
					if r.Method != "POST" || r.URL.Query().Get("accessToken") != "access-secret" {
						t.Fatal("换取会话请求错误")
					}
					return uploadTestResponse(200, `{"res_code":0,"sessionKey":"fresh-pc-session"}`), nil
				case "/api/open/user/getUserInfoForPortal.action":
					if r.URL.Query().Get("sessionKey") != "fresh-pc-session" {
						t.Fatal("未换取新会话 Cookie")
					}
					resp := uploadTestResponse(200, `{"res_code":0}`)
					resp.Header.Add("Set-Cookie", "COOKIE_LOGIN_USER=fresh-cookie; Path=/; Secure")
					return resp, nil
				case "/api/portal/v2/getUserBriefInfo.action":
					c, e := r.Cookie("COOKIE_LOGIN_USER")
					if e != nil || c.Value != "fresh-cookie" {
						t.Fatal("未使用新 Cookie")
					}
					return uploadTestResponse(200, `{"sessionKey":"fresh-web-session"}`), nil
				case "/api/security/generateRsaKey.action":
					b, _ := json.Marshal(map[string]string{"pubKey": base64.StdEncoding.EncodeToString(der), "pkId": "fresh-key"})
					return uploadTestResponse(200, string(b)), nil
				default:
					t.Fatalf("意外请求: %s", r.URL.Path)
					return nil, nil
				}
			})}
			var result json.RawMessage
			err := w.webRequest("/person/initMultiUpload", map[string]string{"fileSize": "1073741824"}, &result)
			if (err != nil) != failAgain {
				t.Fatalf("结果不正确: %v", err)
			}
			if refreshes != 1 || inits != 2 {
				t.Fatalf("恢复未限制一次: refresh=%d init=%d", refreshes, inits)
			}
		})
	}
}
func TestNonInitErrorDoesNotRefreshSession(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/person/getMultiUploadUrls", "/person/commitMultiUploadFile"} {
		count := 0
		w := &webUploader{sessionKey: "session", publicKey: &key.PublicKey, pkID: "key", accessToken: "access"}
		w.client = &http.Client{Transport: uploadTestTransport(func(r *http.Request) (*http.Response, error) {
			count++
			if r.URL.Path != path {
				t.Fatal("非初始化失败不应重建会话")
			}
			return uploadTestResponse(500, `{"error":"Internal Server Error"}`), nil
		})}
		if err := w.webRequest(path, nil, nil); err == nil || count != 1 {
			t.Fatalf("请求重复或错误丢失: %v", err)
		}
	}
}
func TestSessionTransportErrorDoesNotExposeToken(t *testing.T) {
	w := &webUploader{accessToken: "private-access-token", client: &http.Client{Transport: uploadTestTransport(func(r *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: errors.New("network unavailable")}
	})}}
	err := w.refreshUploadSession()
	if err == nil || strings.Contains(err.Error(), "private-access-token") || strings.Contains(err.Error(), "accessToken=") {
		t.Fatalf("认证信息泄漏: %v", err)
	}
}
