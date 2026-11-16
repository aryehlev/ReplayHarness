package discover

import "github.com/aryehlev/ReplayHarness/internal/model"

func req(method, path, query, body string) model.Request {
	r := model.Request{Method: method, Path: path, Query: query, Body: body}
	if body != "" {
		r.Headers = map[string]string{"Content-Type": "application/json"}
	}
	return r
}
