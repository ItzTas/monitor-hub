package notify

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	httpTimeout     = 30 * time.Second
	maxAnswerLength = 4 << 10
)

var errBadRequest = errors.New("could not build the request")

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: httpTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func post(client *http.Client, request *http.Request) (int, []byte, error) {
	response, err := client.Do(request)
	if err != nil {
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err
		}
		return 0, nil, fmt.Errorf("could not reach the server: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(response.Body, maxAnswerLength))
	if err != nil {
		return 0, nil, fmt.Errorf("could not read the answer: %w", err)
	}
	return response.StatusCode, answer, nil
}

func expectSuccess(status int) error {
	if status >= 200 && status < 300 {
		return nil
	}
	return fmt.Errorf("the server answered %d", status)
}
