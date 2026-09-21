package panupload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tickstep/cloudpan189-api/cloudpan"
	"github.com/tickstep/cloudpan189-api/cloudpan/apiutil"
)

// webHTTPError 保留状态码，以便仅对初始化接口的已知会话异常做一次恢复。
type webHTTPError struct {
	status int
	cause  error
}

func (e *webHTTPError) Error() string { return fmt.Sprintf("HTTP %d: %v", e.status, e.cause) }
func (e *webHTTPError) Unwrap() error { return e.cause }

// refreshUploadSession 创建仅供本次上传使用的新会话，不覆盖用户持久化配置。
// 部分会话仍可列目录，但上传初始化持续返回 500；不能仅根据浏览接口成功判断上传会话有效。
func (w *webUploader) refreshUploadSession() error {
	if w.accessToken == "" {
		return errors.New("缺少 accessToken，无法自动恢复上传会话，请重新登录")
	}
	var session cloudpan.AppLoginToken
	endpoint := "https://api.cloud.189.cn/getSessionForPC.action?appId=8025431004&" + apiutil.PcClientInfoSuffixParam() + "&accessToken=" + url.QueryEscape(w.accessToken)
	if _, err := w.sessionRequest(http.MethodPost, endpoint, &session); err != nil {
		return fmt.Errorf("换取新上传会话失败: %w", err)
	}
	if session.SessionKey == "" {
		return errors.New("换取新上传会话失败: 未返回 sessionKey")
	}
	endpoint = "https://cloud.189.cn/api/open/user/getUserInfoForPortal.action?sessionKey=" + url.QueryEscape(session.SessionKey)
	resp, err := w.sessionRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("换取新网页凭据失败: %w", err)
	}
	cookie := ""
	for _, c := range resp.Cookies() {
		if c.Name == "COOKIE_LOGIN_USER" && c.Value != "" {
			cookie = c.Value
		}
	}
	if cookie == "" {
		return errors.New("换取新网页凭据失败: 未返回登录 Cookie")
	}
	w.cookieLoginUser = cookie
	if session.AccessToken != "" {
		w.accessToken = session.AccessToken
	}
	w.sessionKey, w.pkID = "", ""
	w.publicKey = nil
	return nil
}

// sessionRequest 不在错误信息中输出携带 accessToken/sessionKey 的 URL。
func (w *webUploader) sessionRequest(method, endpoint string, result interface{}) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, errors.New("无法构造会话请求")
	}
	req.Header.Set("Accept", "application/json;charset=UTF-8")
	resp, err := w.client.Do(req)
	if err != nil {
		for {
			var ue *url.Error
			if !errors.As(err, &ue) {
				break
			}
			err = ue.Err
		}
		return nil, fmt.Errorf("会话请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var apierr webRespErr
	if json.Unmarshal(body, &apierr) == nil && apierr.hasError() {
		message := apierr.toError().Error()
		for _, secret := range []string{w.accessToken, req.URL.Query().Get("sessionKey")} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[已隐藏]")
			}
		}
		return nil, &webHTTPError{resp.StatusCode, errors.New(message)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &webHTTPError{resp.StatusCode, errors.New(http.StatusText(resp.StatusCode))}
	}
	if result != nil {
		if err = json.Unmarshal(body, result); err != nil {
			return nil, fmt.Errorf("解析会话响应失败: %w", err)
		}
	}
	return resp, nil
}
