// Copyright (c) 2020 tickstep.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package cmder

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tickstep/cloudpan189-api/cloudpan"
	"github.com/tickstep/library-go/logger"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// 扫码登录(扫码认证)状态码, 来自 open.e.189.cn 统一登录框前端逻辑
const (
	// qrLoginStatusSuccess 扫码登录成功, 响应带 redirectUrl
	qrLoginStatusSuccess = 0
	// qrLoginStatusWaiting 等待扫码
	qrLoginStatusWaiting = -106
	// qrLoginStatusScanned 已扫码, 等待手机端确认
	qrLoginStatusScanned = -11002
	// qrLoginStatusExpired 二维码已过期
	qrLoginStatusExpired = -11001
	// qrLoginStatusSecondVerify 需要二次验证
	qrLoginStatusSecondVerify = -134
)

// 扫码登录相关地址与参数。
// 注: 依赖的 cloudpan189-api v0.1.0 未导出 APP_ID/CLIENT_TYPE/RETURN_URL/PC 等常量,
// 且其 AUTH_URL 已带 /api/logbox/oauth2 后缀, 故在此按统一登录框(v4.1)实际值本地定义。
const (
	qrLoginOauth2Host = "https://open.e.189.cn" // 统一登录框域名
	qrLoginApiHost    = "https://api.cloud.189.cn"
	qrLoginAppId      = "8025431004"
	qrLoginClientType = "10020"
	qrLoginReturnUrl  = "https://m.cloud.189.cn/zhuanti/2020/loginErrorPc/index.html"
	qrLoginPc         = "TELEPC"
	qrLoginVersion    = "6.2"
	qrLoginChannelId  = "web_cloud.189.cn"
)

// 扫码轮询间隔与超时时间
const (
	qrLoginPollInterval = 3 * time.Second
	qrLoginPollTimeout  = 3 * time.Minute
	qrLoginUserAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

// qrUuidResp getUUID.do 响应
type qrUuidResp struct {
	Result     int    `json:"result"`
	Msg        string `json:"msg"`
	Uuid       string `json:"uuid"`       // 二维码内容
	EncryUuid  string `json:"encryuuid"`  // 轮询二维码状态用
	EncodeUuid string `json:"encodeuuid"` // 拼接二维码图片地址用
}

// qrLoginStateResp qrcodeLoginState.do 轮询响应
type qrLoginStateResp struct {
	Status      int    `json:"status"`
	RedirectUrl string `json:"redirectUrl"`
	ApToken     string `json:"apToken"`
}

// qrSessionResp getSessionForPC.action 响应, 换取 APP 会话信息
type qrSessionResp struct {
	ResCode             int    `json:"res_code"`
	ResMessage          string `json:"res_message"`
	AccessToken         string `json:"accessToken"`
	FamilySessionKey    string `json:"familySessionKey"`
	FamilySessionSecret string `json:"familySessionSecret"`
	LoginName           string `json:"loginName"`
	RefreshToken        string `json:"refreshToken"`
	SessionKey          string `json:"sessionKey"`
	SessionSecret       string `json:"sessionSecret"`
}

// qrLoginFormCache 统一登录框页面中解析出来的登录参数
type qrLoginFormCache struct {
	reqId   string
	lt      string
	paramId string
}

// qrHttpClient 扫码登录专用的 http 客户端:
// 基于 DefaultTransport 克隆, 保留 HTTP/2 能力(登录页服务已强制 HTTP/2, 纯 HTTP/1.1 会超时/挂起),
// 自带 cookie jar 以保存 LT 等登录 cookie。
type qrHttpClient struct {
	client *http.Client
	// redirectHeader 跟随重定向时附加到每个跳转请求上的自定义头。
	// Go 默认在跨域重定向时会丢弃自定义头(如 lt), 而服务端可能各跳都校验 lt。
	redirectHeader map[string]string
}

func newQrHttpClient() *qrHttpClient {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = true
	jar, _ := cookiejar.New(nil)
	h := &qrHttpClient{}
	h.client = &http.Client{
		Transport: tr,
		Jar:       jar,
		Timeout:   60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("重定向次数过多")
			}
			for k, v := range h.redirectHeader {
				req.Header.Set(k, v)
			}
			// 保持 Referer 指向上一个跳转地址
			if prev := via[len(via)-1]; prev != nil {
				req.Header.Set("Referer", prev.URL.String())
			}
			return nil
		},
	}
	return h
}

// setRedirectHeader 设置/清除(传 nil)重定向时附加的请求头
func (qc *qrHttpClient) setRedirectHeader(header map[string]string) {
	qc.redirectHeader = header
}

// get 发起 GET 请求, 返回响应体
func (qc *qrHttpClient) get(urlStr string, header map[string]string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", qrLoginUserAgent)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := qc.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// postForm 以表单方式发起 POST 请求, 返回响应体
func (qc *qrHttpClient) postForm(urlStr string, form map[string]string, header map[string]string) ([]byte, error) {
	params := url.Values{}
	for k, v := range form {
		params.Set(k, v)
	}
	req, err := http.NewRequest(http.MethodPost, urlStr, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", qrLoginUserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := qc.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// cookies 返回指定域名下的 cookie
func (qc *qrHttpClient) cookies(scheme, host string) []*http.Cookie {
	return qc.client.Jar.Cookies(&url.URL{Scheme: scheme, Host: host, Path: "/"})
}

// setCookies 向指定域名写入 cookie (如 LT)
func (qc *qrHttpClient) setCookies(host string, cookies []*http.Cookie) {
	qc.client.Jar.SetCookies(&url.URL{Scheme: "https", Host: host, Path: "/"}, cookies)
}

// QrCodeLogin 扫码认证登录流程。
// 账号开启扫码验证后, APP 密码登录 (unifyLoginForPC.action) 会失败/超时,
// 此时可改用此方式: 与服务端生成二维码, 用户使用天翼云盘 App 扫码并确认后,
// 再换取 APP 会话 (sessionKey) 与 WEB 会话 (COOKIE_LOGIN_USER)。
func QrCodeLogin() (username string, webToken cloudpan.WebLoginToken, appToken cloudpan.AppLoginToken, err error) {
	client := newQrHttpClient()

	// 1. 获取登录参数 (reqId/lt/paramId)
	loginForm, err := fetchQrLoginForm(client)
	if err != nil {
		fmt.Printf("获取扫码登录参数失败: %s\n", err)
		return "", webToken, appToken, err
	}

	// 2. 生成二维码
	uuidResp, err := getQrCodeUuid(client, loginForm)
	if err != nil {
		fmt.Printf("获取二维码失败: %s\n", err)
		return "", webToken, appToken, err
	}

	// 3. 下载二维码图片到本地临时目录, 优先在终端直接显示二维码, 供用户扫码
	savePath, err := saveQrLoginImage(client, uuidResp.EncodeUuid, loginForm.reqId)
	if err != nil {
		fmt.Printf("保存二维码图片失败: %s\n", err)
		return "", webToken, appToken, err
	}
	fmt.Printf("请在手机上打开天翼云盘App, 使用\"扫码登录\"扫描以下二维码完成登录:\n")
	// Windows 的 cmd 控制台(GBK 代码页)无法显示 ▀/▄/█ 半块字符, 保留打开图片文件的旧方式
	if runtime.GOOS != "windows" && renderQrLoginImage(savePath) == nil {
		// 终端已直接打印二维码; 图片文件保留作为备用
		fmt.Printf("如二维码显示异常, 可打开以下图片扫码:\n%s\n\n", savePath)
	} else {
		// 终端渲染失败(如图片格式异常/无法识别)或为 Windows, 回退为提示打开图片文件
		logger.Verboseln("render qr login image to terminal: failed, fallback to image file")
		fmt.Printf("打开以下路径, 以查看二维码\n%s\n\n", savePath)
	}
	fmt.Printf("若仍无法扫码, 可直接访问以下链接查看二维码:\n%s\n\n", uuidResp.Uuid)

	// 4. 轮询二维码状态
	si, err := pollQrLoginState(client, loginForm, uuidResp)
	if err != nil {
		return "", webToken, appToken, err
	}
	logger.Verboseln("qr login success redirectUrl: " + si.RedirectUrl)

	// 5. WEB 会话 cookie 可能由轮询成功响应直接下发, 先查一次 jar
	qrDumpJarCookies(client, "afterPoll")
	webToken.CookieLoginUser = qrCookieLoginUserFromJar(client)

	// 6. 换取 APP 会话信息 (sessionKey/sessionSecret)
	session, err := getQrCodeAppSession(client, si.RedirectUrl)
	if err != nil {
		return "", webToken, appToken, err
	}
	appToken = cloudpan.AppLoginToken{
		SessionKey:          session.SessionKey,
		SessionSecret:       session.SessionSecret,
		FamilySessionKey:    session.FamilySessionKey,
		FamilySessionSecret: session.FamilySessionSecret,
		AccessToken:         session.AccessToken,
		RefreshToken:        session.RefreshToken,
	}
	username = session.LoginName
	logger.Verboseln("getSessionForPC ok: loginName=" + session.LoginName +
		", sessionKey=" + session.SessionKey + ", sessionSecret=" + session.SessionSecret)

	// 7. 用 APP 会话的 sessionKey 调用 cloud.189.cn 的 web 接口,
	// 触发服务器在响应中下发 COOKIE_LOGIN_USER。
	// 注: 服务端已废弃 ssoLogin.action 换 cookie 的老方式, 现改为随 web 接口响应下发。
	if webToken.CookieLoginUser == "" {
		webToken.CookieLoginUser = qrWebSessionCookie(client, session.SessionKey)
	}

	// 8. 若取不到, 再模拟浏览器跟随二维码登录的 redirectUrl, 落 WEB cookie
	if webToken.CookieLoginUser == "" {
		logger.Verboseln("try to follow redirectUrl: " + si.RedirectUrl)
		redirectHeader := map[string]string{
			"lt":      loginForm.lt,
			"Referer": qrLoginOauth2Host,
		}
		client.setRedirectHeader(redirectHeader)
		data, getErr := client.get(si.RedirectUrl, redirectHeader)
		client.setRedirectHeader(nil)
		if getErr != nil {
			logger.Verboseln("follow redirectUrl error: ", getErr.Error())
		} else {
			logger.Verboseln("follow redirectUrl resp size: ", len(data))
		}
		qrDumpJarCookies(client, "redirectUrl")
		webToken.CookieLoginUser = qrCookieLoginUserFromJar(client)
	}

	logger.Verboseln("qr login final cookie: COOKIE_LOGIN_USER=" + webToken.CookieLoginUser)

	// 9. 没有 WEB 会话时直接报错, 避免带着空 cookie 进入后续业务逻辑
	if webToken.CookieLoginUser == "" {
		return "", webToken, appToken, errors.New("获取WEB会话(COOKIE_LOGIN_USER)失败, 请重新登录再试")
	}
	return
}

// qrWebSessionCookie 通过携带 sessionKey 调用 cloud.189.cn 的 web 接口,
// 触发服务器在响应中下发 COOKIE_LOGIN_USER, 再从 cookie jar 中取出。
func qrWebSessionCookie(client *qrHttpClient, sessionKey string) string {
	if sessionKey == "" {
		return ""
	}
	header := map[string]string{"Accept": "application/json;charset=UTF-8"}
	urls := []string{
		fmt.Sprintf("%s/api/open/user/getUserInfoForPortal.action?sessionKey=%s", cloudpan.WEB_URL, url.QueryEscape(sessionKey)),
		fmt.Sprintf("%s/v2/getUserDetailInfo.action?sessionKey=%s", cloudpan.WEB_URL, url.QueryEscape(sessionKey)),
	}
	for _, u := range urls {
		logger.Verboseln("do request url: " + u)
		if _, err := client.get(u, header); err != nil {
			logger.Verboseln("web session cookie request error: ", err.Error())
			continue
		}
		if c := qrCookieLoginUserFromJar(client); c != "" {
			return c
		}
	}
	return ""
}

// qrDumpJarCookies 调试用: 打印 cookie jar 中各域名的 cookie 名称与值
func qrDumpJarCookies(client *qrHttpClient, tag string) {
	for _, host := range []string{"cloud.189.cn", "m.cloud.189.cn", "open.e.189.cn"} {
		for _, cookie := range client.cookies("https", host) {
			logger.Verboseln(fmt.Sprintf("[%s] cookie[%s] %s=%s", tag, host, cookie.Name, cookie.Value))
		}
	}
}

// qrCookieLoginUserFromJar 从 cookie jar 中查找 COOKIE_LOGIN_USER
func qrCookieLoginUserFromJar(client *qrHttpClient) string {
	for _, host := range []string{"cloud.189.cn", "m.cloud.189.cn"} {
		for _, cookie := range client.cookies("https", host) {
			if cookie.Name == "COOKIE_LOGIN_USER" && cookie.Value != "" {
				return cookie.Value
			}
		}
	}
	return ""
}

// fetchQrLoginForm 请求登录框页面, 解析 reqId/lt/paramId 等登录参数。
// 使用 /api/portal 前缀的新登录页地址; 该地址会 302 到 open.e.189.cn 的统一登录框,
// 部分网络下响应较慢, 失败时自动重试。
func fetchQrLoginForm(client *qrHttpClient) (*qrLoginFormCache, error) {
	var lastErr error
	for i := 0; i < 3; i++ {
		fullUrl := fmt.Sprintf("%s/api/portal/unifyLoginForPC.action?appId=%s&clientType=%s&returnURL=%s&timeStamp=%d",
			cloudpan.WEB_URL, qrLoginAppId, qrLoginClientType, url.QueryEscape(qrLoginReturnUrl), time.Now().UnixMilli())
		logger.Verboseln("do request url: " + fullUrl)
		data, err := client.get(fullUrl, map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
		})
		if err != nil {
			lastErr = err
			logger.Verboseln("fetch login form error: ", err.Error())
			continue
		}
		content := string(data)
		lc := &qrLoginFormCache{}
		if m := regexp.MustCompile(`lt = "([^"]+)"`).FindStringSubmatch(content); len(m) > 1 {
			lc.lt = m[1]
		}
		if m := regexp.MustCompile(`paramId = "([^"]+)"`).FindStringSubmatch(content); len(m) > 1 {
			lc.paramId = m[1]
		}
		if m := regexp.MustCompile(`reqId = "([^"]+)"`).FindStringSubmatch(content); len(m) > 1 {
			lc.reqId = m[1]
		}
		if lc.reqId == "" || lc.lt == "" || lc.paramId == "" {
			logger.Verboseln("fetch login form: parse params failed")
			lastErr = errors.New("解析登录参数失败")
			continue
		}
		// 与浏览器一致: 把登录链路所需的 LT cookie 写入 cookie jar
		ltCookie := []*http.Cookie{{Name: "LT", Value: lc.lt, Path: "/"}}
		client.setCookies("open.e.189.cn", ltCookie)
		client.setCookies("cloud.189.cn", ltCookie)
		logger.Verboseln("set LT cookie into jar: " + lc.lt)
		return lc, nil
	}
	return nil, lastErr
}

// getQrCodeUuid 调用 getUUID.do 生成二维码
func getQrCodeUuid(client *qrHttpClient, lc *qrLoginFormCache) (*qrUuidResp, error) {
	header := map[string]string{
		"Referer": qrLoginOauth2Host,
		"REQID":   lc.reqId,
		"lt":      lc.lt,
	}
	body, err := client.postForm(qrLoginOauth2Host+"/api/logbox/oauth2/getUUID.do",
		map[string]string{"appId": qrLoginAppId}, header)
	if err != nil {
		logger.Verboseln("getUUID.do error: ", err.Error())
		return nil, err
	}
	logger.Verboseln("getUUID.do response: " + string(body))
	uuidResp := &qrUuidResp{}
	if err := json.Unmarshal(body, uuidResp); err != nil {
		return nil, err
	}
	if uuidResp.Result != 0 || uuidResp.Uuid == "" || uuidResp.EncryUuid == "" {
		return nil, fmt.Errorf("生成二维码失败: %s", uuidResp.Msg)
	}
	return uuidResp, nil
}

// saveQrLoginImage 从 image.do 下载二维码图片保存到临时目录
func saveQrLoginImage(client *qrHttpClient, encodeUuid, reqId string) (string, error) {
	imgUrl := fmt.Sprintf("%s/api/logbox/oauth2/image.do?uuid=%s&REQID=%s", qrLoginOauth2Host, encodeUuid, reqId)
	logger.Verboseln("try to download qr login image: ", imgUrl)
	imgContents, err := client.get(imgUrl, nil)
	if err != nil {
		return "", err
	}
	savePath := filepath.Join(os.TempDir(), fmt.Sprintf("qrlogin_%d.png", time.Now().UnixMilli()))
	if err := os.WriteFile(savePath, imgContents, 0777); err != nil {
		return "", err
	}
	return savePath, nil
}

// pollQrLoginState 轮询二维码扫码状态, 直至登录成功或失败
func pollQrLoginState(client *qrHttpClient, lc *qrLoginFormCache, uuidResp *qrUuidResp) (*qrLoginStateResp, error) {
	var (
		deadline      = time.Now().Add(qrLoginPollTimeout)
		lastStatus    = qrLoginStatusWaiting
		scannedPrompt = false
	)
	for time.Now().Before(deadline) {
		si, err := checkQrLoginState(client, lc, uuidResp)
		if err != nil {
			return nil, err
		}
		switch si.Status {
		case qrLoginStatusSuccess:
			logger.Verboseln("qr login success")
			return si, nil
		case qrLoginStatusExpired:
			return nil, errors.New("二维码已过期, 请重新登录再试")
		case qrLoginStatusSecondVerify:
			return nil, errors.New("账号需要二次验证, 请改用账号密码登录方式重试")
		case qrLoginStatusScanned:
			if !scannedPrompt {
				fmt.Println("已扫码, 请在手机上确认登录...")
				scannedPrompt = true
			}
		case qrLoginStatusWaiting:
			// 等待扫码中
		default:
			// 未知状态, 继续轮询
		}
		if si.Status != lastStatus {
			logger.Verboseln("qr login status: ", si.Status)
			lastStatus = si.Status
		}
		time.Sleep(qrLoginPollInterval)
	}
	return nil, errors.New("扫码登录超时, 请重试")
}

func checkQrLoginState(client *qrHttpClient, lc *qrLoginFormCache, uuidResp *qrUuidResp) (*qrLoginStateResp, error) {
	now := time.Now()
	// 与网页端登录框一致的日期格式: yyyy-MM-ddHH:mm:ss + 0~23 随机后缀
	date := now.Format("2006-01-0215:04:05") + strconv.Itoa(rand.Intn(24))
	data := map[string]string{
		"appId":      qrLoginAppId,
		"clientType": "1",
		"returnUrl":  qrLoginReturnUrl,
		"paramId":    lc.paramId,
		"uuid":       uuidResp.Uuid,
		"encryuuid":  uuidResp.EncryUuid,
		"date":       date,
		"timeStamp":  strconv.FormatInt(now.UnixMilli(), 10),
	}
	header := map[string]string{
		"Referer": qrLoginOauth2Host,
		"REQID":   lc.reqId,
		"lt":      lc.lt,
	}
	body, err := client.postForm(qrLoginOauth2Host+"/api/logbox/oauth2/qrcodeLoginState.do", data, header)
	if err != nil {
		logger.Verboseln("qrcodeLoginState.do error: ", err.Error())
		return nil, err
	}
	logger.Verboseln("qrcodeLoginState.do response: " + string(body))
	si := &qrLoginStateResp{}
	if err := json.Unmarshal(body, si); err != nil {
		return nil, err
	}
	return si, nil
}

// getQrCodeAppSession 通过 getSessionForPC.action 换取 APP 会话信息
func getQrCodeAppSession(client *qrHttpClient, redirectUrl string) (*qrSessionResp, error) {
	fullUrl := fmt.Sprintf("%s/getSessionForPC.action?appId=%s&clientType=%s&version=%s&channelId=%s&rand=%d&redirectURL=%s",
		qrLoginApiHost, qrLoginAppId, qrLoginPc, qrLoginVersion, qrLoginChannelId,
		time.Now().UnixMilli(), url.QueryEscape(redirectUrl))
	logger.Verboseln("do request url: " + fullUrl)
	body, err := client.get(fullUrl, map[string]string{
		"Accept": "application/json;charset=UTF-8",
	})
	if err != nil {
		logger.Verboseln("getQrCodeAppSession error: ", err.Error())
		return nil, err
	}
	logger.Verboseln("response: " + string(body))
	session := &qrSessionResp{}
	if err := json.Unmarshal(body, session); err != nil {
		return nil, fmt.Errorf("解析APP会话信息失败: %s", err)
	}
	if session.ResCode != 0 {
		return nil, fmt.Errorf("获取APP会话信息失败: %s", session.ResMessage)
	}
	return session, nil
}
