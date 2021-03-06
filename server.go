package gemdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/GeertJohan/go.rice"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

type Server struct {
	config    *Config
	backend   Backend
	auth      *Auth
	loginHtml []byte
}

func NewServer(config *Config) (*Server, error) {

	multiBackend := NewMultiBackend()

	for _, dir := range config.Dirs {
		dirName := filepath.Base(dir)
		subCacheDir := filepath.Join(config.CacheDir, dirName)
		fsBackend, err := NewFileSystemBackend(dir, subCacheDir)
		if err != nil {
			return nil, err
		}
		multiBackend.AddBackend(filepath.Base(dir), fsBackend)
	}

	if config.RcloneDir != "" {
		rcloneBackend := NewRcloneBackend()
		multiBackend.AddBackend(config.RcloneDir, rcloneBackend)
	}

	auth, err := NewAuth(config.DataDir, config)
	if err != nil {
		return nil, err
	}

	return &Server{
		config:  config,
		backend: multiBackend,
		auth:    auth,
	}, nil
}

func (s *Server) Run(ctx context.Context) error {

	mux := &http.ServeMux{}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {

		box, err := rice.FindBox("files")
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		s.loginHtml, err = box.Bytes("login.html")
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		header := w.Header()

		header["Access-Control-Allow-Origin"] = []string{"*"}
		header["Access-Control-Allow-Methods"] = []string{"*"}
		header["Access-Control-Allow-Headers"] = []string{"*"}
		if r.Method == "OPTIONS" {
			return
		}

		reqPath := r.URL.Path

		hostname := r.Header.Get("X-Forwarded-Host")
		if hostname == "" {
			hostname = r.Host
		}

		if mapRoot, exists := s.config.DomainMap[hostname]; exists {
			reqPath = mapRoot + reqPath
		}

		logLine := fmt.Sprintf("%s\t%s\t%s", r.Method, hostname, reqPath)
		fmt.Println(logLine)

		pathParts := strings.Split(reqPath, "gemdrive/")

		ext := path.Ext(reqPath)
		contentType := mime.TypeByExtension(ext)
		header.Set("Content-Type", contentType)

		if len(pathParts) == 2 {
			s.handleGemDriveRequest(w, r, reqPath)
		} else {
			switch r.Method {
			case "HEAD":
				s.handleHead(w, r, reqPath)
			case "GET":
				s.serveItem(w, r, reqPath)
			case "PUT":
				// TODO: return HTTP 409 if already exists
				s.handlePut(w, r, reqPath)
			case "PATCH":
				// TODO: return HTTP 409 if already exists
				s.handlePatch(w, r, reqPath)
			case "DELETE":
				s.handleDelete(w, r, reqPath)
			}
		}
	})

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.config.Port),
		Handler: mux,
	}

	serverDone := make(chan error)

	go func() {
		err := httpServer.ListenAndServe()
		serverDone <- err
	}()

	select {
	case err := <-serverDone:
		return err
	case <-ctx.Done():
		err := httpServer.Shutdown(ctx)
		return err
	}

	return nil
}

func (s *Server) handleHead(w http.ResponseWriter, r *http.Request, reqPath string) {

	token, _ := extractToken(r)

	header := w.Header()

	if !s.auth.CanRead(token, reqPath) {
		s.sendLoginPage(w, r)
		return
	}

	parentDir := filepath.Dir(reqPath) + "/"

	item, err := s.backend.List(parentDir, 1)
	if e, ok := err.(*Error); ok {
		w.WriteHeader(e.HttpCode)
		w.Write([]byte(e.Message))
		return
	} else if err != nil {
		w.WriteHeader(500)
		w.Write([]byte(err.Error()))
		return
	}

	filename := filepath.Base(reqPath)

	child, exists := item.Children[filename]
	if !exists {
		w.WriteHeader(404)
		io.WriteString(w, "Not found")
		return
	}

	header.Set("Content-Length", fmt.Sprintf("%d", child.Size))
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, reqPath string) {

	token, _ := extractToken(r)

	query := r.URL.Query()

	if !s.auth.CanWrite(token, reqPath) {
		s.sendLoginPage(w, r)
		return
	}

	backend, ok := s.backend.(WritableBackend)

	if !ok {
		w.WriteHeader(500)
		io.WriteString(w, "Backend does not support writing")
		return
	}

	isDir := strings.HasSuffix(reqPath, "/")

	if isDir {
		recursive := query.Get("recursive") == "true"
		err := backend.MakeDir(reqPath, recursive)
		if err != nil {
			w.WriteHeader(400)
			io.WriteString(w, err.Error())
			return
		}
	} else {
		var offset int64 = 0
		truncate := true
		overwrite := query.Get("overwrite") == "true"

		// TODO: consider allowing 0-length files
		if r.ContentLength < 1 {
			w.WriteHeader(400)
			io.WriteString(w, "Invalid write size")
			return
		}

		err := backend.Write(reqPath, r.Body, offset, r.ContentLength, overwrite, truncate)
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}
	}
}

func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request, reqPath string) {

	token, _ := extractToken(r)

	query := r.URL.Query()

	if !s.auth.CanWrite(token, reqPath) {
		s.sendLoginPage(w, r)
		return
	}

	backend, ok := s.backend.(WritableBackend)

	if !ok {
		w.WriteHeader(500)
		io.WriteString(w, "Backend does not support writing")
		return
	}

	overwrite := true
	truncate := false

	offsetParam := query.Get("offset")

	var offset int
	if offsetParam == "" {
		offset = 0
	} else {

		var err error
		offset, err = strconv.Atoi(query.Get("offset"))
		if err != nil {
			w.WriteHeader(400)
			io.WriteString(w, "Invalid offset")
			return
		}
	}

	size, err := strconv.Atoi(r.Header.Get("Content-Length"))
	if err != nil {
		w.WriteHeader(400)
		io.WriteString(w, "Invalid content length")
		return
	}

	err = backend.Write(reqPath, r.Body, int64(offset), int64(size), overwrite, truncate)
	if err != nil {
		w.WriteHeader(500)
		io.WriteString(w, err.Error())
		return
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, reqPath string) {
	token, _ := extractToken(r)

	query := r.URL.Query()

	if !s.auth.CanWrite(token, reqPath) {
		s.sendLoginPage(w, r)
		return
	}

	backend, ok := s.backend.(WritableBackend)

	if !ok {
		w.WriteHeader(500)
		io.WriteString(w, "Backend does not support writing")
		return
	}

	recursive := query.Get("recursive") == "true"
	err := backend.Delete(reqPath, recursive)
	if err != nil {
		w.WriteHeader(500)
		io.WriteString(w, err.Error())
		return
	}
}

func (s *Server) sendLoginPage(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("WWW-Authenticate", "emauth realm=\"Everything\", charset=\"UTF-8\"")
	header.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(403)
	w.Write(s.loginHtml)
}

func (s *Server) handleGemDriveRequest(w http.ResponseWriter, r *http.Request, reqPath string) {

	token, _ := extractToken(r)

	pathParts := strings.Split(reqPath, "gemdrive/")

	gemPath := pathParts[0]
	gemReq := pathParts[1]

	if gemReq == "authorize" {

		s.authorize(w, r)

		return
	}

	if !s.auth.CanRead(token, gemPath) {
		s.sendLoginPage(w, r)
		return
	}

	if gemReq == "meta.json" {

		depth := 1
		depthParam := r.URL.Query().Get("depth")
		if depthParam != "" {
			var err error
			depth, err = strconv.Atoi(depthParam)
			if err != nil {
				w.WriteHeader(400)
				w.Write([]byte("Invalid depth param"))
				return
			}
		}

		item, err := s.backend.List(gemPath, depth)
		if e, ok := err.(*Error); ok {
			w.WriteHeader(e.HttpCode)
			w.Write([]byte(e.Message))
			return
		} else if err != nil {
			w.WriteHeader(500)
			w.Write([]byte(err.Error()))
			return
		}

		jsonBody, err := json.Marshal(item)
		//jsonBody, err := json.MarshalIndent(item, "", "  ")
		if err != nil {
			w.WriteHeader(500)
			w.Write([]byte(err.Error()))
			return
		}

		w.Write(jsonBody)
	} else {
		gemReqParts := strings.Split(gemReq, "/")
		if gemReqParts[0] == "images" {

			if b, ok := s.backend.(ImageServer); ok {
				size, err := strconv.Atoi(gemReqParts[1])
				if err != nil {
					w.WriteHeader(400)
					w.Write([]byte(err.Error()))
					return
				}

				filename := gemReqParts[2]
				imagePath := path.Join(gemPath, filename)
				img, _, err := b.GetImage(imagePath, size)
				if err != nil {
					w.WriteHeader(500)
					w.Write([]byte(err.Error()))
					return
				}

				_, err = io.Copy(w, img)
				if err != nil {
					fmt.Println(err)
				}
			}
		}
	}
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {

	query := r.URL.Query()
	id := query.Get("id")
	code := query.Get("code")

	if id != "" && code != "" {
		token, err := s.auth.CompleteAuth(id, code)
		if err != nil {
			w.WriteHeader(400)
			io.WriteString(w, err.Error())
			return
		}

		cookie := &http.Cookie{
			Name:  "access_token",
			Value: token,
			// TODO: enable Secure
			//Secure:   true,
			HttpOnly: true,
			MaxAge:   86400 * 365,
			Path:     "/",
			SameSite: http.SameSiteLaxMode,
		}
		http.SetCookie(w, cookie)

		io.WriteString(w, token)

	} else {
		bodyJson, err := ioutil.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(400)
			io.WriteString(w, err.Error())
			return
		}

		var key Key
		err = json.Unmarshal(bodyJson, &key)
		if err != nil {
			w.WriteHeader(400)
			io.WriteString(w, err.Error())
			return
		}

		authId, err := s.auth.Authorize(key)
		if err != nil {
			w.WriteHeader(400)
			io.WriteString(w, err.Error())
			return
		}

		io.WriteString(w, authId)
	}
}

func (s *Server) serveItem(w http.ResponseWriter, r *http.Request, reqPath string) {

	token, _ := extractToken(r)

	if !s.auth.CanRead(token, reqPath) {
		s.sendLoginPage(w, r)
		return
	}

	isDir := strings.HasSuffix(reqPath, "/")

	if isDir {
		s.serveDir(w, r, reqPath)
	} else {
		s.serveFile(w, r, reqPath)
	}
}

func (s *Server) serveDir(w http.ResponseWriter, r *http.Request, reqPath string) {
	// If the directory contains an index.html file, serve that by default.
	// Otherwise reading a directory is an error.
	htmlIndexPath := reqPath + "index.html"
	_, data, err := s.backend.Read(htmlIndexPath, 0, 0)
	if err != nil {
		w.WriteHeader(400)
		io.WriteString(w, "Attempted to read directory")
		return
	}

	_, err = io.Copy(w, data)
	if err != nil {
		fmt.Println(err)
	}
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, reqPath string) {

	query := r.URL.Query()

	header := w.Header()
	header.Set("Accept-Ranges", "bytes")

	download := query.Get("download") == "true"
	if download {
		header.Set("Content-Disposition", "attachment")
	}

	rangeHeader := r.Header.Get("Range")

	var offset int64 = 0
	var copyLength int64 = 0

	var rang *HttpRange
	if rangeHeader != "" {
		var err error
		rang, err = parseRange(rangeHeader)
		if err != nil {
			w.WriteHeader(500)
			w.Write([]byte(err.Error()))
			return
		}

		offset = rang.Start

		if rang.End != MAX_INT64 {
			copyLength = rang.End - rang.Start + 1
		}

	}

	item, data, err := s.backend.Read(reqPath, offset, copyLength)
	if readErr, ok := err.(*Error); ok {
		w.WriteHeader(readErr.HttpCode)
		w.Write([]byte(readErr.Message))
		return
	} else if err != nil {
		w.WriteHeader(500)
		w.Write([]byte(err.Error()))
		return
	}
	defer data.Close()

	if rang != nil {
		end := rang.End
		if end == MAX_INT64 {
			end = item.Size - 1
		}
		l := end - rang.Start + 1
		header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rang.Start, end, item.Size))
		header.Set("Content-Length", fmt.Sprintf("%d", l))
		w.WriteHeader(206)
	} else {
		header.Set("Content-Length", fmt.Sprintf("%d", item.Size))
	}

	_, err = io.Copy(w, data)
	if err != nil {
		fmt.Println(err)
	}
}

type HttpRange struct {
	Start int64 `json:"start"`
	// Note: if end is 0 it won't be included in the json because of omitempty
	End int64 `json:"end,omitempty"`
}

// TODO: parse byte range specs properly according to
// https://tools.ietf.org/html/rfc7233
const MAX_INT64 int64 = 9223372036854775807

func parseRange(header string) (*HttpRange, error) {

	parts := strings.Split(header, "=")
	if len(parts) != 2 {
		return nil, errors.New("Invalid Range header")
	}

	rangeParts := strings.Split(parts[1], "-")
	if len(rangeParts) != 2 {
		return nil, errors.New("Invalid Range header")
	}

	var start int64 = 0
	if rangeParts[0] != "" {
		var err error
		start, err = strconv.ParseInt(rangeParts[0], 10, 64)
		if err != nil {
			return nil, err
		}
	}

	var end int64 = MAX_INT64
	if rangeParts[1] != "" {
		var err error
		end, err = strconv.ParseInt(rangeParts[1], 10, 64)
		if err != nil {
			return nil, err
		}
	}

	return &HttpRange{
		Start: start,
		End:   end,
	}, nil
}

// Looks for auth token in cookie, then header, then query string
func extractToken(r *http.Request) (string, error) {
	tokenName := "access_token"

	query := r.URL.Query()

	queryToken := query.Get(tokenName)
	if queryToken != "" {
		return queryToken, nil
	}

	authHeader := r.Header.Get("Authorization")

	if authHeader != "" {
		tokenHeader := strings.Split(authHeader, " ")[1]
		return tokenHeader, nil
	}

	tokenCookie, err := r.Cookie(tokenName)

	if err == nil {
		return tokenCookie.Value, nil
	}

	return "", errors.New("No token found")
}
