package network

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/jianyu-im/JianYuServerLib/pkg/util"
	"github.com/sendgrid/rest"
)

func Post(url string, body []byte, headers map[string]string) (resp *rest.Response, err error) {

	return RequestBoy(url, body, headers, rest.Post)
}

func Put(url string, body []byte, headers map[string]string) (resp *rest.Response, err error) {

	return RequestBoy(url, body, headers, rest.Put)
}

func PostForQueryParam(url string, queryParams map[string]string, headers map[string]string) (resp *rest.Response, err error) {

	return RequestBoyForQueryParam(url, queryParams, headers, rest.Post)
}

func RequestBoyForQueryParam(url string, queryParams map[string]string, headers map[string]string, method rest.Method) (resp *rest.Response, err error) {

	request := rest.Request{
		Method:      method,
		BaseURL:     url,
		QueryParams: queryParams,
		Headers:     headers,
	}
	response, err := rest.API(request)
	if err != nil {
		return nil, err
	}

	return response, nil
}
func RequestBoy(url string, body []byte, headers map[string]string, method rest.Method) (resp *rest.Response, err error) {

	request := rest.Request{
		Method:  method,
		BaseURL: url,
		Body:    body,
		Headers: headers,
	}
	response, err := rest.API(request)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func Get(url string, queryParams map[string]string, headers map[string]string) (resp *rest.Response, err error) {

	request := rest.Request{
		Method:      rest.Get,
		BaseURL:     url,
		Headers:     headers,
		QueryParams: queryParams,
	}
	response, err := rest.API(request)
	if err != nil {

		return nil, err
	}

	return response, nil
}

func GetJson(url string, queryParams map[string]string, headers map[string]string) (byts []byte, err error) {

	request := rest.Request{
		Method:      rest.Get,
		BaseURL:     url,
		Headers:     headers,
		QueryParams: queryParams,
	}
	response, err := rest.API(request)
	if err != nil {

		return nil, err
	}

	return []byte(response.Body), nil
}

// PostForWWWFormForBytres 以 application/x-www-form-urlencoded 提交表单
//
// 2026-08-25 修复：原实现构造了 url.Values 却弃之不用，改用 fmt.Sprintf 手工拼
// "k=v&" —— 值完全不做百分号编码。只要任意一个值里出现 '%'（消息正文里的涨跌幅
// "1.55%" 最常见），接收端按 urlencoded 解码就会遇到非法转义序列，把**整个表单**
// 判为解析失败：小米返回 65011 "Title or Description is empty"、OPPO 返回 41
// "Invalid Arguments"，线上每天因此丢掉约 2.3 万条离线推送。
// url.Values.Encode() 会正确编码每个值，同时对纯 ASCII 参数（OAuth 的
// client_id/secret、签名 hex 串）输出与原来完全一致，因此对其余调用方无行为变化。
func PostForWWWFormForBytres(urlStr string, params map[string]string, headers map[string]string) ([]byte, error) {
	data := url.Values{}
	for key, value := range params {
		data.Set(key, value)
	}
	request, err := http.NewRequest("POST", urlStr, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var resp *http.Response
	resp, err = http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return body, errors.New(fmt.Sprintf("状态码：%d", resp.StatusCode))
	}
	return body, nil
}

func PostForWWWForm(urlStr string, params map[string]string, headers map[string]string) (map[string]interface{}, error) {
	body, err := PostForWWWFormForBytres(urlStr, params, headers)
	if err != nil {
		return nil, err
	}
	var resultMap map[string]interface{}
	err = util.ReadJsonByByte(body, &resultMap)
	if err != nil {
		return nil, err
	}
	return resultMap, nil

}

func PostForWWWFormForAll(urlStr string, bodyData io.Reader, headers map[string]string) ([]byte, error) {
	request, err := http.NewRequest("POST", urlStr, bodyData)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var resp *http.Response
	resp, err = http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(fmt.Sprintf("状态码：%d", resp.StatusCode))
	}
	return body, nil
}

func PostForWWWFormReXML(urlStr string, params map[string]string, headers map[string]string) ([]byte, error) {

	data := url.Values{}
	for key, value := range params {
		data.Set(key, value)
	}
	queryStr := ""

	for key, value := range params {
		queryStr = fmt.Sprintf("%s=%s&%s", key, value, queryStr)
	}
	if len(queryStr) > 0 {
		queryStr = queryStr[0 : len(queryStr)-1]
	}
	request, err := http.NewRequest("POST", urlStr, strings.NewReader(queryStr))
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var resp *http.Response
	resp, err = http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respData, err := ioutil.ReadAll(resp.Body)
	fmt.Println(string(respData))
	if err != nil {
		return []byte(""), err
	}
	return respData, nil

}
