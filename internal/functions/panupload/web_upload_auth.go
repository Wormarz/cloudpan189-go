package panupload

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tickstep/cloudpan189-api/cloudpan/apiutil"
)

// loadWebAuth 按官网流程建立网页上传会话并获取该会话的 RSA 公钥。
// 仅使用已保存的 Cookie，不在 URL 或日志中携带认证信息。
func (w *webUploader) loadWebAuth() error {
	if w.publicKey != nil {
		return nil
	}
	if w.cookieLoginUser == "" {
		return errors.New("缺少网页登录凭据，请重新登录")
	}
	var brief struct {
		SessionKey string `json:"sessionKey"`
	}
	if err := w.webAuthGet("/api/portal/v2/getUserBriefInfo.action", &brief); err != nil {
		return fmt.Errorf("获取网页上传会话失败: %w", err)
	}
	if brief.SessionKey == "" {
		return errors.New("未返回网页上传会话，请重新登录")
	}
	var key struct {
		PubKey string `json:"pubKey"`
		PkID   string `json:"pkId"`
	}
	if err := w.webAuthGet("/api/security/generateRsaKey.action", &key); err != nil {
		return fmt.Errorf("获取上传公钥失败: %w", err)
	}
	if key.PubKey == "" || key.PkID == "" {
		return errors.New("上传公钥响应不完整")
	}
	var der []byte
	if block, _ := pem.Decode([]byte(key.PubKey)); block != nil {
		der = block.Bytes
	} else {
		var err error
		der, err = base64.StdEncoding.DecodeString(key.PubKey)
		if err != nil {
			return fmt.Errorf("解析上传公钥失败: %w", err)
		}
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return fmt.Errorf("解析上传公钥失败: %w", err)
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return errors.New("上传公钥不是 RSA 类型")
	}
	w.sessionKey, w.publicKey, w.pkID = brief.SessionKey, pub, key.PkID
	return nil
}

func (w *webUploader) webAuthGet(path string, result interface{}) error {
	req, err := http.NewRequest(http.MethodGet, "https://cloud.189.cn"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json;charset=UTF-8")
	req.Header.Set("Referer", "https://cloud.189.cn/web/main/")
	req.AddCookie(&http.Cookie{Name: "COOKIE_LOGIN_USER", Value: w.cookieLoginUser})
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求网页认证接口失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var apierr webRespErr
	if json.Unmarshal(body, &apierr) == nil && apierr.hasError() {
		return fmt.Errorf("HTTP %d: %w", resp.StatusCode, apierr.toError())
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if err = json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("解析网页认证响应失败: %w", err)
	}
	return nil
}

// newWebRequest 使用随机 AES 密钥加密参数，RSA 包装临时密钥，并以临时密钥签名。
// 协议对应官网网页上传及 cloud189-sdk 的 signatureUpload。
func (w *webUploader) newWebRequest(path string, params map[string]string) (*http.Request, error) {
	if err := w.loadWebAuth(); err != nil {
		return nil, err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	secret := hex.EncodeToString(random)
	enc, err := aesEcbEncrypt(webJoinParams(params), secret[:16])
	if err != nil {
		return nil, err
	}
	enc = strings.ToLower(enc)
	encryptedKey, err := rsa.EncryptPKCS1v15(rand.Reader, w.publicKey, []byte(secret))
	if err != nil {
		return nil, err
	}
	date := fmt.Sprint(time.Now().UnixMilli())
	req, err := http.NewRequest(http.MethodGet, webUploadBaseUrl+path+"?"+url.Values{"params": {enc}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json;charset=UTF-8")
	req.Header.Set("Referer", "https://cloud.189.cn/web/main/")
	req.Header.Set("SessionKey", w.sessionKey)
	req.Header.Set("X-Request-Date", date)
	req.Header.Set("X-Request-ID", strings.ToLower(apiutil.XRequestId()))
	req.Header.Set("EncryptionText", base64.StdEncoding.EncodeToString(encryptedKey))
	req.Header.Set("PkId", w.pkID)
	req.Header.Set("Signature", strings.ToLower(webHmacSign(secret, w.sessionKey, http.MethodGet, path, date, enc)))
	return req, nil
}
