// Command azupload uploads a file to an Azurite (or Azure) container using
// SharedKey auth. It exists only so the E2E suite can seed Azurite without
// the Azure CLI.
//
//	go run ./test/e2e/azupload <endpoint> <account> <base64-key> <container> <blob> <file>
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args) != 7 {
		fmt.Fprintln(os.Stderr, "usage: azupload <endpoint> <account> <base64-key> <container> <blob> <file>")
		os.Exit(2)
	}
	endpoint, account, key, container, blob, file := strings.TrimRight(os.Args[1], "/"), os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6]
	data, err := os.ReadFile(file)
	if err != nil {
		fail(err)
	}
	kb, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		fail(err)
	}

	// Creating an existing container returns 409, which is fine.
	send(kb, account, http.MethodPut, endpoint+"/"+container+"?restype=container", nil, "", "\nrestype:container")

	segs := strings.Split(blob, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	if code := send(kb, account, http.MethodPut, endpoint+"/"+container+"/"+strings.Join(segs, "/"), data, "BlockBlob", ""); code != http.StatusCreated {
		fail(fmt.Errorf("upload returned %d", code))
	}
}

func send(key []byte, account, method, rawURL string, body []byte, blobType, canonQuery string) int {
	req, err := http.NewRequest(method, rawURL, bytes.NewReader(body))
	if err != nil {
		fail(err)
	}
	now := time.Now().UTC().Format(http.TimeFormat)
	req.Header.Set("x-ms-date", now)
	req.Header.Set("x-ms-version", "2020-10-02")
	headers := ""
	if blobType != "" {
		req.Header.Set("x-ms-blob-type", blobType)
		headers = "x-ms-blob-type:" + blobType + "\n"
	}
	headers += "x-ms-date:" + now + "\nx-ms-version:2020-10-02"
	length := ""
	if len(body) > 0 {
		length = strconv.Itoa(len(body))
	}
	sts := fmt.Sprintf("%s\n\n\n%s\n\n\n\n\n\n\n\n\n%s\n/%s%s%s", method, length, headers, account, req.URL.EscapedPath(), canonQuery)
	h := hmac.New(sha256.New, key)
	h.Write([]byte(sts))
	req.Header.Set("Authorization", "SharedKey "+account+":"+base64.StdEncoding.EncodeToString(h.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "azupload:", err)
	os.Exit(1)
}
