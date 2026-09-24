package liverunner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHLSCookiesAreIsolatedAcrossRenditions(t *testing.T) {
	store, _, response, _ := mediaTestSessionV1(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rendition := strings.Split(r.URL.Path, "/")[3]
		cookie, err := r.Cookie("mediamtx")
		if err != nil {
			// Same name and broad path across muxers reproduces the production failure.
			http.SetCookie(w, &http.Cookie{Name: "mediamtx", Value: rendition, Path: "/", Secure: true})
			http.Redirect(w, r, r.URL.Path+"?cookieCheck=1", http.StatusFound)
			return
		}
		if cookie.Value != rendition {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, "#EXTM3U\n")
	}))
	defer upstream.Close()
	handler, err := NewHLSHandlerV1(store, testLivePresetsV1(), upstream.URL, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, rendition := range []string{"720p", "360p", "720p", "360p"} {
		if _, err := handler.fetchPlaylist(context.Background(), response.RunnerSessionID, "renditions/"+response.RunnerSessionID+"/"+rendition+"/index.m3u8"); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rendition := []string{"720p", "360p"}[i%2]
			for _, asset := range []string{"video1_stream.m3u8", "audio2_stream.m3u8"} {
				if _, err := handler.fetchPlaylist(context.Background(), response.RunnerSessionID, "renditions/"+response.RunnerSessionID+"/"+rendition+"/"+asset); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
	handler.forgetSessionCookies(response.RunnerSessionID)
	if len(handler.cookies) != 0 {
		t.Fatal("session cookies leaked")
	}
}

func TestPublicHLSCORSOnErrorsAndPreflight(t *testing.T) {
	store, _, _, _ := mediaTestSessionV1(t)
	handler, err := NewHLSHandlerV1(store, testLivePresetsV1(), "http://127.0.0.1:8888", nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		r := httptest.NewRequest(method, "/v1/public/sessions/missing/master.m3u8", nil)
		r.Header.Set("Origin", "https://portal.example")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Header().Get("Access-Control-Allow-Origin") != "*" || w.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatal("invalid public asset CORS")
		}
		if method == http.MethodOptions && (w.Code != http.StatusNoContent || !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "Range")) {
			t.Fatal("preflight failed")
		}
	}
}
