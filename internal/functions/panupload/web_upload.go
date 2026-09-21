// Copyright (c) 2020 tickstep.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package panupload

import (
	"bytes"
	"crypto/aes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/tickstep/cloudpan189-api/cloudpan"
	"github.com/tickstep/cloudpan189-go/internal/taskframework"
	"github.com/tickstep/library-go/converter"
)

const (
	// maxSingleUploadSize 天翼云盘数据上传接口对单个 PUT 请求有恰好 200MiB(209715200 字节)的上限,
	// 超过该大小的文件单个请求必然返回 413, 必须改用 upload.cloud.189.cn 的分片上传接口。
	maxSingleUploadSize = 200 * 1024 * 1024

	// webUploadBaseUrl 天翼云盘 web 端分片上传接口域名
	webUploadBaseUrl = "https://upload.cloud.189.cn"

	// webUploadDefaultSliceSize 分片基准大小 10MiB
	webUploadDefaultSliceSize = 10 * 1024 * 1024
)

// webStreamUpload 大文件(>200MiB)通过 upload.cloud.189.cn 分片上传接口上传。
// 流程: initMultiUpload -> 分片读取并逐个获取预签名URL上传 -> commitMultiUploadFile。
// 使用官网网页上传协议：Cookie 获取网页会话、公钥；临时密钥加密并签名，RSA 包装密钥。
func (utu *UploadTaskUnit) webStreamUpload() (result *taskframework.TaskUnitRunResult) {
	result = &taskframework.TaskUnitRunResult{}
	fileSize := utu.LocalFileChecksum.Length
	if fileSize <= maxSingleUploadSize {
		result.Err = errors.New("文件大小未超过单次上传上限, 无需分片上传")
		result.ResultMessage = "文件大小未超过单次上传上限"
		return
	}
	if utu.LocalFileChecksum.ParentFolderId == "" {
		result.Err = errors.New("未获取到目标目录ID")
		result.ResultMessage = "未获取到目标目录ID"
		return
	}

	// 打开并定位本地文件
	file, err := utu.LocalFileChecksum.GetFile(), error(nil)
	if file == nil {
		result.Err = errors.New("打开本地文件失败")
		result.ResultMessage = "打开本地文件失败"
		return
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		result.Err = err
		result.ResultMessage = "定位本地文件失败"
		return
	}

	sliceSize := webPartSize(fileSize)
	count := int((fileSize + sliceSize - 1) / sliceSize)
	lastSize := fileSize % sliceSize
	if lastSize == 0 {
		lastSize = sliceSize
	}

	uploader := newWebUploader(utu.WebToken, utu.AppToken)
	pathPrefix := "/person"
	params := map[string]string{
		"parentFolderId": utu.LocalFileChecksum.ParentFolderId,
		"fileName":       url.QueryEscape(utu.panFile),
		"fileSize":       fmt.Sprint(fileSize),
		"sliceSize":      fmt.Sprint(sliceSize),
		"lazyCheck":      "1",
	}
	if utu.FamilyId > 0 {
		pathPrefix = "/family"
		params["familyId"] = fmt.Sprint(utu.FamilyId)
	}

	// 1. 初始化分片上传
	var initResp struct {
		Data struct {
			UploadFileID   string `json:"uploadFileId"`
			FileDataExists int    `json:"fileDataExists"`
		} `json:"data"`
	}
	if err := uploader.webRequest(pathPrefix+"/initMultiUpload", params, &initResp); err != nil {
		result.ResultMessage = "初始化分片上传失败:" + err.Error()
		result.Err = err
		result.NeedRetry = true
		return
	}
	uploadFileId := initResp.Data.UploadFileID
	if uploadFileId == "" {
		result.Err = errors.New("初始化分片上传失败: 未返回 uploadFileId")
		result.ResultMessage = result.Err.Error()
		result.NeedRetry = true
		return
	}
	cmdUploadVerbose.Infof("initMultiUpload ok uploadFileId=%s sliceSize=%d parts=%d", uploadFileId, sliceSize, count)

	// 2. 分片读取并上传, 同时累计各分片 md5 用于最终校验
	var (
		partMd5Hexes = make([]string, 0, count)
		uploaded     int64
	)
	for i := 1; i <= count; i++ {
		slice := make([]byte, sliceSize)
		if i == count {
			slice = slice[:lastSize]
		}
		if _, err := io.ReadFull(file, slice); err != nil {
			result.Err = err
			result.ResultMessage = "读取本地文件分片失败"
			result.NeedRetry = true
			return
		}
		partMd5 := md5.Sum(slice)
		partMd5Hexes = append(partMd5Hexes, strings.ToUpper(hex.EncodeToString(partMd5[:])))
		partInfo := fmt.Sprintf("%d-%s", i, base64.StdEncoding.EncodeToString(partMd5[:]))

		var urlsResp struct {
			UploadUrls map[string]webUploadUrl `json:"uploadUrls"`
		}
		if err := uploader.webRequest(pathPrefix+"/getMultiUploadUrls", map[string]string{
			"uploadFileId": uploadFileId,
			"partInfo":     partInfo,
		}, &urlsResp); err != nil {
			result.Err = fmt.Errorf("获取分片%d上传地址失败: %w", i, err)
			result.ResultMessage = result.Err.Error()
			result.NeedRetry = true
			return
		}
		partUrl, ok := urlsResp.UploadUrls[fmt.Sprintf("partNumber_%d", i)]
		if !ok {
			result.Err = fmt.Errorf("获取分片%d上传地址失败: 未返回对应地址", i)
			result.ResultMessage = result.Err.Error()
			result.NeedRetry = true
			return
		}
		if err := uploader.putSlice(partUrl.RequestURL, partUrl.RequestHeader, slice); err != nil {
			result.Err = fmt.Errorf("上传分片%d失败: %w", i, err)
			result.ResultMessage = result.Err.Error()
			result.NeedRetry = true
			return
		}

		uploaded += int64(len(slice))
		if utu.ShowProgress {
			fmt.Printf("\r[%s] ↑ %s/%s in %d 分片 (%d/%d) ............", utu.taskInfo.Id(),
				converter.ConvertFileSize(uploaded, 2),
				converter.ConvertFileSize(fileSize, 2),
				count, i, count,
			)
		}
	}

	// 3. 提交合并
	fileMd5Hex := strings.ToUpper(utu.LocalFileChecksum.MD5)
	sliceMd5Hex := fileMd5Hex
	if fileSize > sliceSize {
		sum := md5.Sum([]byte(strings.Join(partMd5Hexes, "\n")))
		sliceMd5Hex = strings.ToUpper(hex.EncodeToString(sum[:]))
	}
	opertype := "1"
	if utu.IsOverwrite {
		opertype = "3"
	}
	var commitResp struct {
		File struct {
			UserFileID string `json:"userFileId"`
		} `json:"file"`
	}
	if err := uploader.webRequest(pathPrefix+"/commitMultiUploadFile", map[string]string{
		"uploadFileId": uploadFileId,
		"fileMd5":      fileMd5Hex,
		"sliceMd5":     sliceMd5Hex,
		"lazyCheck":    "1",
		"isLog":        "0",
		"opertype":     opertype,
	}, &commitResp); err != nil {
		result.Err = fmt.Errorf("提交分片上传失败: %w", err)
		result.ResultMessage = result.Err.Error()
		result.NeedRetry = true
		return
	}
	if commitResp.File.UserFileID == "" {
		result.Err = errors.New("提交分片上传失败: 未返回文件ID")
		result.ResultMessage = result.Err.Error()
		return
	}
	cmdUploadVerbose.Infof("commitMultiUploadFile ok fileId=%s", commitResp.File.UserFileID)

	// 成功: 统计 + 数据库清理 + 打印结果
	fmt.Printf("\n")
	fmt.Printf("[%s] 上传文件成功, 保存到网盘路径: %s\n", utu.taskInfo.Id(), utu.SavePath)
	utu.UploadStatistic.AddTotalSize(fileSize)
	utu.UploadingDatabase.Delete(&utu.LocalFileChecksum.LocalFileMeta)
	utu.UploadingDatabase.Save()
	result.Succeed = true
	return
}

// webPartSize 计算分片大小: 默认 10MiB; 超过约9.77GiB用20MiB片; 超过约19.5GiB按最多1999片推算。
func webPartSize(fileSize int64) int64 {
	const def = int64(webUploadDefaultSliceSize)
	switch {
	case fileSize > def*2*999:
		n := (fileSize + 1999*def - 1) / (1999 * def)
		if n < 5 {
			n = 5
		}
		return n * def
	case fileSize > def*999:
		return def * 2
	default:
		return def
	}
}

// webUploader upload.cloud.189.cn 分片上传客户端
type webUploader struct {
	client          *http.Client
	sessionKey      string
	cookieLoginUser string
	accessToken     string
	publicKey       *rsa.PublicKey
	pkID            string
}

func newWebUploader(webToken cloudpan.WebLoginToken, appToken cloudpan.AppLoginToken) *webUploader {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = true
	tr.ResponseHeaderTimeout = 60 * time.Second
	return &webUploader{client: &http.Client{Transport: tr}, cookieLoginUser: webToken.CookieLoginUser, accessToken: appToken.AccessToken}
}

// webRequest 请求 upload.cloud.189.cn 接口(GET)。
// 业务参数用临时密钥加密成 params，密钥由 RSA 包装，使用官网网页上传认证。
func (w *webUploader) webRequest(path string, params map[string]string, result interface{}) error {
	err := w.webRequestOnce(path, params, result)
	var apierr *webHTTPError
	if !strings.HasSuffix(path, "/initMultiUpload") || !errors.As(err, &apierr) || apierr.status != http.StatusInternalServerError {
		return err
	}
	if refreshErr := w.refreshUploadSession(); refreshErr != nil {
		return fmt.Errorf("%w；自动恢复上传会话失败: %v", err, refreshErr)
	}
	cmdUploadVerbose.Infof("已刷新上传会话，重试初始化")
	return w.webRequestOnce(path, params, result)
}

func (w *webUploader) webRequestOnce(path string, params map[string]string, result interface{}) error {
	req, err := w.newWebRequest(path, params)
	if err != nil {
		return err
	}

	cmdUploadVerbose.Infof("web upload request: %s", path)
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	cmdUploadVerbose.Infof("web upload response: %s HTTP %d", path, resp.StatusCode)

	// 服务端错误(JSON/XML 混合结构)
	var perr webRespErr
	if err := json.Unmarshal(body, &perr); err == nil && perr.hasError() {
		return &webHTTPError{resp.StatusCode, perr.toError()}
	}
	if resp.StatusCode != http.StatusOK {
		return &webHTTPError{resp.StatusCode, errors.New(http.StatusText(resp.StatusCode))}
	}
	if result != nil {
		if err := json.Unmarshal(body, result); err != nil {
			return fmt.Errorf("解析接口响应失败: %v", err)
		}
	}
	return nil
}

// putSlice 上传单个分片到预签名URL, 使用请求头中原样带上的校验头, 不需要再加签名。
// 注意: 预签名URL已包含签名所需查询参数, 不能再追加任何参数(否则 SignatureDoesNotMatch)。
func (w *webUploader) putSlice(reqUrl, reqHeader string, data []byte) error {
	fullUrl := reqUrl
	req, err := http.NewRequest(http.MethodPut, fullUrl, bytes.NewReader(data))
	if err != nil {
		return err
	}
	for k, v := range webParseHttpHeader(reqHeader) {
		req.Header.Set(k, v)
	}
	cmdUploadVerbose.Infof("upload slice: size=%d", len(data))
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var perr webRespErr
	if err := json.Unmarshal(body, &perr); err == nil && perr.hasError() {
		return &webHTTPError{resp.StatusCode, perr.toError()}
	}
	if resp.StatusCode != http.StatusOK {
		return &webHTTPError{resp.StatusCode, errors.New(http.StatusText(resp.StatusCode))}
	}
	return nil
}

// --- 加密与签名辅助 ---

// aesEcbEncrypt AES-128-ECB + PKCS7 加密, 输出大写hex
func aesEcbEncrypt(data, key string) (string, error) {
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", err
	}
	src := webPkcs7Pad([]byte(data), block.BlockSize())
	dst := make([]byte, len(src))
	for off := 0; off < len(src); off += aes.BlockSize {
		block.Encrypt(dst[off:off+aes.BlockSize], src[off:off+aes.BlockSize])
	}
	return strings.ToUpper(hex.EncodeToString(dst)), nil
}

func webPkcs7Pad(src []byte, blockSize int) []byte {
	padding := blockSize - len(src)%blockSize
	return append(src, bytes.Repeat([]byte{byte(padding)}, padding)...)
}

// webHmacSign HMAC-SHA1 签名, 与官方一致: 明文含 params(加密后的) 时追加 &params=。
func webHmacSign(sessionSecret, sessionKey, operate, fullUrl, dateOfGmt, params string) string {
	requestUri := fullUrl
	if idx := strings.Index(requestUri, "?"); idx >= 0 {
		requestUri = requestUri[:idx]
	}
	requestUri = strings.ReplaceAll(requestUri, "https://", "")
	requestUri = strings.ReplaceAll(requestUri, "http://", "")
	if idx := strings.Index(requestUri, "/"); idx >= 0 {
		requestUri = requestUri[idx:]
	}
	plain := fmt.Sprintf("SessionKey=%s&Operate=%s&RequestURI=%s&Date=%s", sessionKey, operate, requestUri, dateOfGmt)
	if params != "" {
		plain += "&params=" + params
	}
	mac := hmac.New(sha1.New, []byte(sessionSecret))
	mac.Write([]byte(plain))
	return strings.ToUpper(hex.EncodeToString(mac.Sum(nil)))
}

// webJoinParams 参数按 key 排序后拼接为 k=v&k=v
func webJoinParams(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
	}
	return b.String()
}

// webParseHttpHeader 解析预签名请求头字符串 (k=v&k=v 格式)
func webParseHttpHeader(str string) map[string]string {
	h := map[string]string{}
	for _, pair := range strings.Split(str, "&") {
		if k, v, ok := strings.Cut(pair, "="); ok && k != "" {
			h[k] = v
		}
	}
	return h
}

// webRespErr 服务端错误响应
type webRespErr struct {
	ResCode    any    `json:"res_code"`
	ResMessage string `json:"res_message"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	ErrorCode  string `json:"errorCode"`
	ErrorMsg   string `json:"errorMsg"`
	Msg        string `json:"msg"`
	Error_     string `json:"error"`
}

func (e webRespErr) hasError() bool {
	switch v := e.ResCode.(type) {
	case float64:
		if v != 0 {
			return true
		}
	case string:
		if v != "" && v != "0" {
			return true
		}
	}
	return (e.Code != "" && e.Code != "SUCCESS") || e.ErrorCode != "" || e.Error_ != ""
}

func (e webRespErr) toError() error {
	msg := e.ResMessage
	if msg == "" {
		msg = e.Msg
	}
	if msg == "" {
		msg = e.Message
	}
	if msg == "" {
		msg = e.ErrorMsg
	}
	if msg == "" {
		msg = e.Error_
	}
	code := e.Code
	if e.ErrorCode != "" {
		code = e.ErrorCode
	}
	if e.ResCode != nil && fmt.Sprint(e.ResCode) != "" && fmt.Sprint(e.ResCode) != "0" {
		code = fmt.Sprint(e.ResCode)
	}
	if code != "" && code != "SUCCESS" {
		if msg == "" {
			msg = code
		} else if msg != code {
			msg = code + ": " + msg
		}
	}
	if msg == "" {
		msg = e.ErrorCode
	}
	if msg == "" {
		msg = e.Code
	}
	if msg == "" {
		msg = "服务器返回错误"
	}
	return errors.New(msg)
}

// webUploadUrl 单个分片的预签名上传信息
type webUploadUrl struct {
	RequestURL    string `json:"requestURL"`
	RequestHeader string `json:"requestHeader"`
}
