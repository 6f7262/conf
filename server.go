package kipp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gabriel-vasile/mimetype"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/uhthomas/kipp/database"
	"github.com/uhthomas/kipp/filesystem"
	"github.com/zeebo/blake3"
)

// Server acts as the HTTP server and configuration.
type Server struct {
	Database      database.Database
	FileSystem    filesystem.FileSystem
	Lifetime      time.Duration
	Limit         int64
	PublicPath    string
	metricHandler http.Handler
}

func New(ctx context.Context, opts ...Option) (*Server, error) {
	r := prometheus.NewRegistry()
	if err := r.Register(prometheus.NewGoCollector()); err != nil {
		return nil, fmt.Errorf("register go collector: %w", err)
	}
	s := &Server{
		metricHandler: promhttp.InstrumentMetricHandler(
			r, promhttp.HandlerFor(r, promhttp.HandlerOpts{}),
		),
	}
	for _, opt := range opts {
		if err := opt(ctx, s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// ServeHTTP will serve HTTP requests. It first tries to determine if the
// request is for uploading, it then tries to serve static files and then will
// try to serve public files.
func (s Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodPost:
		if r.URL.Path == "/" {
			s.UploadHandler(w, r)
			return
		}
		fallthrough
	default:
		allow := "GET, HEAD, OPTIONS"
		if r.URL.Path == "/" {
			allow = "GET, HEAD, OPTIONS, POST"
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", allow)
		} else {
			w.Header().Set("Allow", allow)
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		}
		return
	}

	switch r.URL.Path {
	case "/healthz":
		s.Health(w, r)
		return
	case "/varz":
		s.metricHandler.ServeHTTP(w, r)
		return
	}

	http.FileServer(fileSystemFunc(func(name string) (_ http.File, err error) {
		if f, err := http.Dir(s.PublicPath).Open(name); !os.IsNotExist(err) {
			d, err := f.Stat()
			if err != nil {
				return nil, err
			}
			if !d.IsDir() {
				w.Header().Set("Cache-Control", "max-age=31536000")
				// nginx style weak Etag
				w.Header().Set("Etag", fmt.Sprintf(
					`W/"%x-%x"`,
					d.ModTime().Unix(), d.Size(),
				))
			}
			return f, nil
		}

		dir, name := path.Split(name)
		if dir != "/" {
			return nil, os.ErrNotExist
		}

		// trim anything after the first "."
		if i := strings.Index(name, "."); i > -1 {
			name = name[:i]
		}

		e, err := s.Database.Lookup(r.Context(), name)
		if err != nil {
			if errors.Is(err, database.ErrNoResults) {
				return nil, os.ErrNotExist
			}
			return nil, err
		}

		cache := "max-age=31536000" // ~ 1 year
		if e.Lifetime != nil {
			now := time.Now()
			if e.Lifetime.Before(now) {
				return nil, os.ErrNotExist
			}
			cache = fmt.Sprintf(
				"public, must-revalidate, max-age=%d",
				int(e.Lifetime.Sub(now).Seconds()),
			)
		}

		f, err := s.FileSystem.Open(r.Context(), e.Slug)
		if err != nil {
			return nil, err
		}
		defer func() {
			if err != nil {
				f.Close()
			}
		}()

		ctype, err := detectContentType(e.Name, f)
		if err != nil {
			return nil, fmt.Errorf("detect content type: %w", err)
		}

		// catches text/html and text/html; charset=utf-8
		const prefix = "text/html"
		if strings.HasPrefix(ctype, prefix) {
			ctype = "text/plain" + ctype[len(prefix):]
		}

		w.Header().Set("Cache-Control", cache)
		w.Header().Set("Content-Disposition", fmt.Sprintf(
			"filename=%q; filename*=UTF-8''%[1]s",
			url.PathEscape(e.Name),
		))
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Etag", strconv.Quote(e.Sum))
		if e.Lifetime != nil {
			w.Header().Set("Expires", e.Lifetime.Format(http.TimeFormat))
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		return &file{Reader: f, entry: e}, nil
	})).ServeHTTP(w, r)
}

func (s Server) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()

	if err := s.Database.Ping(ctx); err != nil {
		log.Printf("ping: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// UploadHandler write the contents of the "file" part to a filesystem.Reader,
// persists the entry to the database and writes the location of the file
// to the response.
func (s Server) UploadHandler(w http.ResponseWriter, r *http.Request) {
	// Due to the overhead of multipart bodies, the actual limit for files
	// is smaller than it should be. It's not really feasible to calculate
	// the overhead so this is *good enough* for the time being.
	//
	// TODO(thomas): is there a better way to limit the size for the
	//      part, rather than the whole body?
	if r.ContentLength > s.Limit {
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.Limit)

	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var p *multipart.Part
	for {
		if p, err = mr.NextPart(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if p.FormName() == "file" {
			break
		}
	}
	defer p.Close()

	name := p.FileName()
	if len(name) > 255 {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}

	var b [9]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	slug := base64.RawURLEncoding.EncodeToString(b[:])

	if err := s.FileSystem.Create(r.Context(), slug, filesystem.PipeReader(func(w io.Writer) error {
		h := blake3.New()
		n, err := io.Copy(io.MultiWriter(w, h), p)
		if err != nil {
			return fmt.Errorf("copy: %w", err)
		}

		now := time.Now()

		var l *time.Time
		if s.Lifetime > 0 {
			t := now.Add(s.Lifetime)
			l = &t
		}

		if err := s.Database.Create(r.Context(), database.Entry{
			Slug:      slug,
			Name:      name,
			Sum:       base64.RawURLEncoding.EncodeToString(h.Sum(nil)),
			Size:      n,
			Timestamp: now,
			Lifetime:  l,
		}); err != nil {
			return fmt.Errorf("create entry: %w", err)
		}
		return nil
	})); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ext := filepath.Ext(name)

	var sb strings.Builder
	sb.Grow(len(slug) + len(ext) + 2)
	sb.WriteRune('/')
	sb.WriteString(slug)
	sb.WriteString(ext)

	http.Redirect(w, r, sb.String(), http.StatusSeeOther)

	sb.WriteRune('\n')
	io.WriteString(w, sb.String())
}

// detectContentType sniffs up-to the first 3072 bytes of the stream,
// falling back to extension if the content type could not be detected.
func detectContentType(name string, r io.ReadSeeker) (string, error) {
	var b [3072]byte
	n, _ := io.ReadFull(r, b[:])
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "", errors.New("seeker can't seek")
	}
	m := mimetype.Detect(b[:n])
	if m.Is("application/octet-stream") {
		if ctype := mime.TypeByExtension(filepath.Ext(name)); ctype != "" {
			return ctype, nil
		}
	}
	return m.String(), nil
}
