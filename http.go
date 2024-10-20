package illumioapi

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var Verbose bool

func init() {
	if strings.ToLower(os.Getenv("ILLUMIOAPI_VERBOSE")) == "true" {
		Verbose = true
	}
}

// verboseLog prints a message to stdout if the ILLUMIOAPI_VERBOSE env variable is set to "true"
func verboseLog(m string) {
	if !Verbose {
		return
	}
	fmt.Printf("%s [ILLUMIOAPI VERBOSE] - %s\r\n", time.Now().Format("2006-01-02 15:04:05 "), m)
}

// verboseLogf prints a message to stdout using string formatting if the ILLUMIOAPI_VERBOSE env variable is set to "true"
func verboseLogf(format string, a ...any) {
	verboseLog(fmt.Sprintf(format, a...))
}

// APIResponse contains the information from the response of the API
type APIResponse struct {
	RespBody   string
	StatusCode int
	Header     http.Header
	Request    *http.Request
	ReqBody    string
	Warnings   []string
}

// Unexported struct for handling the asyncResults
type asyncResults struct {
	Href        string `json:"href"`
	JobType     string `json:"job_type"`
	Description string `json:"description"`
	Result      struct {
		Href string `json:"href"`
	} `json:"result"`
	Status       string `json:"status"`
	RequestedAt  string `json:"requested_at"`
	TerminatedAt string `json:"terminated_at"`
	RequestedBy  struct {
		Href string `json:"href"`
	} `json:"requested_by"`
}

func (p *PCE) httpSetup(action, apiURL string, body []byte, async bool, headers map[string]string) (APIResponse, error) {
	var asyncResults asyncResults

	// Get the base URL
	u, err := url.Parse(apiURL)
	if err != nil {
		return APIResponse{}, err
	}
	baseURL := "https://" + u.Host + "/api/v2"
	verboseLogf("httpSetup - base url: %s", baseURL)
	verboseLogf("httpSetup - async: %t", async)

	// Create body
	httpBody := bytes.NewBuffer(body)

	// Create HTTP client and request
	client := &http.Client{}

	// Create the http transport obect
	httpTransport := &http.Transport{}
	if p.DisableTLSChecking {
		httpTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	if p.Proxy != "" {
		proxyUrl, err := url.Parse(p.Proxy)
		if err != nil {
			return APIResponse{}, err
		}
		httpTransport.Proxy = http.ProxyURL(proxyUrl)
	}

	// Add to the client
	client.Transport = httpTransport

	req, err := http.NewRequest(action, apiURL, httpBody)
	if err != nil {
		return APIResponse{}, err
	}

	if os.Getenv("FORCE_ASYNC") == "true" {
		async = true
	}

	// Set basic authentication and headers
	req.SetBasicAuth(p.User, p.Key)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if async {
		req.Header.Set("Prefer", "respond-async")
	}

	// Make HTTP Request
	verboseLogf("httpSetup - making %s http request to %s", req.Method, req.URL)
	resp, err := client.Do(req)
	if err != nil {
		return APIResponse{}, err
	}
	defer resp.Body.Close()
	verboseLogf("httpSetup - http status code: %d", resp.StatusCode)

	// Strip base URL for async logging
	targetResource := strings.TrimPrefix(req.URL.String(), baseURL)

	// Process Async requests
	if async {
		verboseLogf("httpSetup - starting async polling process for %s", targetResource)
		iteration := 0
		for asyncResults.Status != "done" {
			iteration++
			verboseLogf("httpSetup - checking async results for %s - attempt %d", targetResource, iteration)
			asyncResults, err = p.asyncPoll(baseURL, resp)
			if err != nil {
				return APIResponse{}, err
			}
		}
		verboseLog("httpSetup - async polling done")

		finalReq, err := http.NewRequest("GET", baseURL+asyncResults.Result.Href, httpBody)
		if err != nil {
			return APIResponse{}, err
		}

		// Set basic authentication and headers
		finalReq.SetBasicAuth(p.User, p.Key)
		finalReq.Header.Set("Content-Type", "application/json")

		// Make HTTP Request
		verboseLogf("httpSetup - making http request to download async results from %s for %s", finalReq.URL.String(), targetResource)
		resp, err = client.Do(finalReq)
		if err != nil {
			return APIResponse{}, err
		}
		defer resp.Body.Close()
		verboseLogf("httpSetup - http status code: %d", resp.StatusCode)

	}

	// Process response
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return APIResponse{}, err
	}

	// Put relevant response info into struct
	var response APIResponse
	response.RespBody = string(data[:])
	response.StatusCode = resp.StatusCode
	response.Header = resp.Header
	response.Request = resp.Request

	// Check for a 200 response code
	if strconv.Itoa(resp.StatusCode)[0:1] != "2" {
		return response, errors.New("http status code of " + strconv.Itoa(response.StatusCode))
	}

	// Return data and nil error
	return response, nil
}

// asyncPoll is used in async requests to check when the data is ready
func (p *PCE) asyncPoll(baseURL string, origResp *http.Response) (asyncResults asyncResults, err error) {

	// Create HTTP client and request
	client := &http.Client{}

	// Create the http transport obect
	httpTransport := &http.Transport{}
	if p.DisableTLSChecking {
		httpTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	if p.Proxy != "" {
		proxyUrl, err := url.Parse(p.Proxy)
		if err != nil {
			return asyncResults, err
		}
		httpTransport.Proxy = http.ProxyURL(proxyUrl)
	}

	// Add to the client
	client.Transport = httpTransport

	pollReq, err := http.NewRequest("GET", baseURL+origResp.Header.Get("Location"), nil)
	verboseLogf("asyncPoll - pollReq.UR.String(): %s", pollReq.URL.String())
	if err != nil {
		return asyncResults, err
	}

	// Set basic authentication and headers
	pollReq.SetBasicAuth(p.User, p.Key)
	pollReq.Header.Set("Content-Type", "application/json")

	// Wait for recommended time from Retry-After
	wait, err := strconv.Atoi(origResp.Header.Get("Retry-After"))
	verboseLogf("asyncPoll - Retry-After: %d", wait)
	if err != nil {
		return asyncResults, err
	}
	duration := time.Duration(wait) * time.Second
	verboseLog("asyncPoll - sleeping for Retry-After period")
	verboseLogf("asyncPoll - duration.Seconds(): %d", int(duration.Seconds()))
	time.Sleep(duration)

	// Check if the data is ready
	verboseLogf("asyncPoll - making http request to %s", pollReq.URL.String())
	pollResp, err := client.Do(pollReq)
	if err != nil {
		return asyncResults, err
	}
	defer pollResp.Body.Close()
	verboseLogf("asyncPoll - http status code: %d", pollResp.StatusCode)

	// Process Response
	data, err := io.ReadAll(pollResp.Body)
	if err != nil {
		return asyncResults, err
	}

	// Put relevant response info into struct
	json.Unmarshal(data[:], &asyncResults)

	return asyncResults, err
}

// httpReq makes an API call to the PCE with sepcified options
// httpAction must be GET, POST, PUT, or DELETE.
// apiURL is the full endpoint being called.
// PUT and POST methods should have a body that is JSON run through the json.marshal function so it's a []byte.
// async parameter should be set to true for any GET requests returning > 500 items.
func (p *PCE) httpReq(action, apiURL string, body []byte, async bool, headers map[string]string) (APIResponse, error) {

	// Make initial http call
	api, err := p.httpSetup(action, apiURL, body, async, headers)
	retry := 0

	// If the status code is 429, try 3 times
	for api.StatusCode == 429 {
		// If we have already tried 3 times, exit
		if retry > 6 {
			return api, errors.New("received 6 429 errors with 30 second pauses between attempts")
		}
		// Increment the retry counter and sleep for 30 seconds
		retry++
		time.Sleep(30 * time.Second)
		// Retry the API call
		api, err = p.httpSetup(action, apiURL, body, async, headers)
	}
	// Return once response code isn't 429
	return api, err
}

// cleanFQDN cleans up the provided PCE FQDN in case of common errors
func (p *PCE) cleanFQDN() string {
	// Remove trailing slash if included
	p.FQDN = strings.TrimSuffix(p.FQDN, "/")
	// Remove HTTPS if included
	p.FQDN = strings.TrimPrefix(p.FQDN, "https://")
	return p.FQDN
}
