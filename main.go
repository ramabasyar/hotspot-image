package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ── Config ────────────────────────────────────────────────────────────────────

type Config struct {
	MinioEndpoint   string // e.g. "localhost:9000"
	MinioAccessKey  string
	MinioSecretKey  string
	MinioBucket     string
	MinioPublicBase string // public URL prefix for images, e.g. "http://192.168.1.10:9000/banners"
	AdminSecret     string // simple secret token to protect upload endpoint
	ListenAddr      string // e.g. ":8080"
	UseSSL          bool
}

func loadConfig() Config {
	// Load .env file (ignore error jika tidak ada, misal di Docker)
	_ = godotenv.Load()

	return Config{
		MinioEndpoint:   getEnv("MINIO_ENDPOINT", "localhost:9000"),
		MinioAccessKey:  getEnv("MINIO_ACCESS_KEY", "minioadmin"),
		MinioSecretKey:  getEnv("MINIO_SECRET_KEY", "minioadmin"),
		MinioBucket:     getEnv("MINIO_BUCKET", "banners"),
		MinioPublicBase: getEnv("MINIO_PUBLIC_BASE", "http://localhost:9000/banners"),
		AdminSecret:     getEnv("ADMIN_SECRET", "ganti-dengan-secret-anda"),
		ListenAddr:      getEnv("LISTEN_ADDR", ":8080"),
		UseSSL:          getEnv("MINIO_USE_SSL", "false") == "true",
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ── Banner ────────────────────────────────────────────────────────────────────

type Banner struct {
	URL          string    `json:"url"`
	Filename     string    `json:"filename"`
	LastModified time.Time `json:"last_modified"`
}

// BannerPair untuk admin UI — pasangan desktop + mobile
type BannerPair struct {
	Slot         int       `json:"slot"`
	Desktop      *Banner   `json:"desktop"`
	Mobile       *Banner   `json:"mobile"`
	LastModified time.Time `json:"last_modified"`
}

// ── Allowed image types ──────────────────────────────────────────────────────

var allowedTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".gif":  "image/gif",
}

func isImageFile(filename string) (string, bool) {
	ext := strings.ToLower(filepath.Ext(filename))
	ct, ok := allowedTypes[ext]
	return ct, ok
}

// ── Server ────────────────────────────────────────────────────────────────────

type Server struct {
	cfg   Config
	minio *minio.Client
}

func NewServer(cfg Config) (*Server, error) {
	mc, err := minio.New(cfg.MinioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.MinioAccessKey, cfg.MinioSecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("minio client: %w", err)
	}

	// Pastikan bucket ada
	ctx := context.Background()
	exists, err := mc.BucketExists(ctx, cfg.MinioBucket)
	if err != nil {
		return nil, fmt.Errorf("check bucket: %w", err)
	}
	if !exists {
		if err := mc.MakeBucket(ctx, cfg.MinioBucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("make bucket: %w", err)
		}
		log.Printf("Bucket '%s' berhasil dibuat", cfg.MinioBucket)

		// Set bucket policy supaya publik (bisa diakses tanpa auth)
		policy := fmt.Sprintf(`{
			"Version":"2012-10-17",
			"Statement":[{
				"Effect":"Allow",
				"Principal":{"AWS":["*"]},
				"Action":["s3:GetObject"],
				"Resource":["arn:aws:s3:::%s/*"]
			}]
		}`, cfg.MinioBucket)
		if err := mc.SetBucketPolicy(ctx, cfg.MinioBucket, policy); err != nil {
			log.Printf("Warning: gagal set bucket policy publik: %v", err)
		}
	}

	return &Server{cfg: cfg, minio: mc}, nil
}

// ── Routes ────────────────────────────────────────────────────────────────────

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// CORS middleware wrapper
	wrap := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Admin-Secret")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("/api/banners", wrap(s.handleBanners))
	mux.HandleFunc("/api/banners/desktop", wrap(s.handleBannersDesktop))
	mux.HandleFunc("/api/banners/mobile", wrap(s.handleBannersMobile))
	mux.HandleFunc("/api/upload", wrap(s.handleUpload))
	mux.HandleFunc("/api/delete", wrap(s.handleDelete))

	// Serve admin UI
	mux.Handle("/admin/", http.StripPrefix("/admin/", http.FileServer(http.Dir("admin"))))

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	return mux
}

// ── GET /api/banners → banner pairs untuk admin (all=true) atau 3 terbaru ────

func (s *Server) handleBanners(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Jika ?all=true, return semua pasangan untuk admin
	if r.URL.Query().Get("all") == "true" {
		s.handleBannersAdmin(w, r)
		return
	}

	// Default: return 3 terbaru dari desktop (backward compatible)
	pairs := s.getBannerPairs(3)

	// Flatten ke format lama
	var banners []Banner
	for _, p := range pairs {
		if p.Desktop != nil {
			banners = append(banners, *p.Desktop)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"banners": banners,
		"total":   len(banners),
	})
}

// handleBannersAdmin → return semua pasangan desktop+mobile
func (s *Server) handleBannersAdmin(w http.ResponseWriter, r *http.Request) {
	pairs := s.getBannerPairs(0) // 0 = semua

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"banners": pairs,
		"total":   len(pairs),
	})
}

// GET /api/banners/desktop → 3 gambar desktop terbaru
func (s *Server) handleBannersDesktop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	banners := s.listBannersByPrefix("desktop/", 3)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"banners": banners,
		"total":   len(banners),
	})
}

// GET /api/banners/mobile → 3 gambar mobile terbaru
func (s *Server) handleBannersMobile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	banners := s.listBannersByPrefix("mobile/", 3)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"banners": banners,
		"total":   len(banners),
	})
}

// ── Helper: list banners by folder prefix ─────────────────────────────────────

func (s *Server) listBannersByPrefix(prefix string, limit int) []Banner {
	ctx := context.Background()
	var banners []Banner

	for obj := range s.minio.ListObjects(ctx, s.cfg.MinioBucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			log.Printf("list object error: %v", obj.Err)
			continue
		}
		if _, ok := isImageFile(obj.Key); !ok {
			continue
		}
		banners = append(banners, Banner{
			URL:          fmt.Sprintf("%s/%s", s.cfg.MinioPublicBase, obj.Key),
			Filename:     obj.Key,
			LastModified: obj.LastModified,
		})
	}

	// Sort terbaru dulu
	sort.Slice(banners, func(i, j int) bool {
		return banners[i].LastModified.After(banners[j].LastModified)
	})

	if limit > 0 && len(banners) > limit {
		banners = banners[:limit]
	}

	return banners
}

// ── Helper: get banner pairs (desktop + mobile matched by timestamp) ──────────

func (s *Server) getBannerPairs(limit int) []BannerPair {
	ctx := context.Background()

	// Collect semua dari kedua folder
	desktopBanners := map[string]Banner{} // key: timestamp part
	mobileBanners := map[string]Banner{}

	for obj := range s.minio.ListObjects(ctx, s.cfg.MinioBucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			continue
		}
		if _, ok := isImageFile(obj.Key); !ok {
			continue
		}

		banner := Banner{
			URL:          fmt.Sprintf("%s/%s", s.cfg.MinioPublicBase, obj.Key),
			Filename:     obj.Key,
			LastModified: obj.LastModified,
		}

		// Extract timestamp prefix dari filename
		// Format: desktop/1717400123_banner.jpg → key = "1717400123"
		base := filepath.Base(obj.Key)            // 1717400123_banner.jpg
		tsPart := strings.SplitN(base, "_", 2)[0] // 1717400123

		if strings.HasPrefix(obj.Key, "desktop/") {
			desktopBanners[tsPart] = banner
		} else if strings.HasPrefix(obj.Key, "mobile/") {
			mobileBanners[tsPart] = banner
		}
	}

	// Build pairs — union semua key
	allKeys := map[string]bool{}
	for k := range desktopBanners {
		allKeys[k] = true
	}
	for k := range mobileBanners {
		allKeys[k] = true
	}

	var pairs []BannerPair
	for ts := range allKeys {
		pair := BannerPair{
			LastModified: time.Time{}, // akan diisi dari yang ada
		}
		if b, ok := desktopBanners[ts]; ok {
			pair.Desktop = &b
			pair.LastModified = b.LastModified
		}
		if b, ok := mobileBanners[ts]; ok {
			pair.Mobile = &b
			if pair.LastModified.IsZero() || b.LastModified.After(pair.LastModified) {
				pair.LastModified = b.LastModified
			}
		}
		pairs = append(pairs, pair)
	}

	// Sort terbaru dulu
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].LastModified.After(pairs[j].LastModified)
	})

	// Assign slot number (1-based)
	for i := range pairs {
		pairs[i].Slot = i + 1
	}

	if limit > 0 && len(pairs) > limit {
		pairs = pairs[:limit]
	}

	return pairs
}

// ── POST /api/upload → upload 2 gambar (desktop + mobile) ────────────────────

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Auth check
	if r.Header.Get("X-Admin-Secret") != s.cfg.AdminSecret {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Parse multipart form (max 40 MB — 2 file × 20 MB)
	if err := r.ParseMultipartForm(40 << 20); err != nil {
		http.Error(w, "file terlalu besar (max 20MB per file)", http.StatusBadRequest)
		return
	}

	// Ambil kedua file
	desktopFile, desktopHeader, err := r.FormFile("desktop")
	if err != nil {
		http.Error(w, "field 'desktop' tidak ditemukan — wajib upload gambar desktop (landscape)", http.StatusBadRequest)
		return
	}
	defer desktopFile.Close()

	mobileFile, mobileHeader, err := r.FormFile("mobile")
	if err != nil {
		http.Error(w, "field 'mobile' tidak ditemukan — wajib upload gambar mobile (portrait)", http.StatusBadRequest)
		return
	}
	defer mobileFile.Close()

	// Validasi tipe file desktop
	desktopCT, ok := isImageFile(desktopHeader.Filename)
	if !ok {
		http.Error(w, "file desktop: tipe file tidak didukung (jpg, png, webp, gif)", http.StatusBadRequest)
		return
	}

	// Validasi tipe file mobile
	mobileCT, ok := isImageFile(mobileHeader.Filename)
	if !ok {
		http.Error(w, "file mobile: tipe file tidak didukung (jpg, png, webp, gif)", http.StatusBadRequest)
		return
	}

	// Nama file unik dengan timestamp yang SAMA supaya pair-nya cocok
	ts := time.Now().UnixMilli()
	desktopObjName := fmt.Sprintf("desktop/%d_%s", ts, desktopHeader.Filename)
	mobileObjName := fmt.Sprintf("mobile/%d_%s", ts, mobileHeader.Filename)

	ctx := context.Background()

	// Upload desktop
	desktopInfo, err := s.minio.PutObject(ctx, s.cfg.MinioBucket, desktopObjName, desktopFile, desktopHeader.Size,
		minio.PutObjectOptions{ContentType: desktopCT})
	if err != nil {
		log.Printf("upload desktop error: %v", err)
		http.Error(w, "gagal upload gambar desktop ke MinIO", http.StatusInternalServerError)
		return
	}

	// Upload mobile
	mobileInfo, err := s.minio.PutObject(ctx, s.cfg.MinioBucket, mobileObjName, mobileFile, mobileHeader.Size,
		minio.PutObjectOptions{ContentType: mobileCT})
	if err != nil {
		// Rollback: hapus file desktop yang sudah terupload
		s.minio.RemoveObject(ctx, s.cfg.MinioBucket, desktopObjName, minio.RemoveObjectOptions{})
		log.Printf("upload mobile error: %v", err)
		http.Error(w, "gagal upload gambar mobile ke MinIO", http.StatusInternalServerError)
		return
	}

	desktopURL := fmt.Sprintf("%s/%s", s.cfg.MinioPublicBase, desktopObjName)
	mobileURL := fmt.Sprintf("%s/%s", s.cfg.MinioPublicBase, mobileObjName)

	log.Printf("Upload sukses: desktop=%s (%d bytes), mobile=%s (%d bytes)",
		desktopObjName, desktopInfo.Size, mobileObjName, mobileInfo.Size)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "upload berhasil",
		"desktop": map[string]interface{}{
			"url":      desktopURL,
			"filename": desktopObjName,
			"size":     desktopInfo.Size,
		},
		"mobile": map[string]interface{}{
			"url":      mobileURL,
			"filename": mobileObjName,
			"size":     mobileInfo.Size,
		},
	})
}

// ── DELETE /api/delete → hapus pasangan banner ────────────────────────────────

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.Header.Get("X-Admin-Secret") != s.cfg.AdminSecret {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var body struct {
		Filename string `json:"filename"` // Bisa desktop/xxx atau mobile/xxx atau timestamp
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Filename == "" {
		http.Error(w, "body harus berisi field 'filename'", http.StatusBadRequest)
		return
	}

	ctx := context.Background()

	// Extract timestamp dari filename
	// Format: desktop/1717400123_banner.jpg → ts = "1717400123"
	base := filepath.Base(body.Filename)
	ts := strings.SplitN(base, "_", 2)[0]

	// Hapus kedua file (desktop + mobile)
	deleted := []string{}
	for _, prefix := range []string{"desktop/", "mobile/"} {
		// Cari file dengan timestamp yang cocok di folder ini
		for obj := range s.minio.ListObjects(ctx, s.cfg.MinioBucket, minio.ListObjectsOptions{
			Prefix:    prefix,
			Recursive: true,
		}) {
			if obj.Err != nil {
				continue
			}
			objBase := filepath.Base(obj.Key)
			objTs := strings.SplitN(objBase, "_", 2)[0]
			if objTs == ts {
				if err := s.minio.RemoveObject(ctx, s.cfg.MinioBucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
					log.Printf("gagal hapus %s: %v", obj.Key, err)
				} else {
					deleted = append(deleted, obj.Key)
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":  "pair berhasil dihapus",
		"deleted":  deleted,
	})
}


// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	cfg := loadConfig()

	srv, err := NewServer(cfg)
	if err != nil {
		log.Fatalf("Gagal inisialisasi server: %v", err)
	}

	log.Printf("🚀 Server berjalan di %s", cfg.ListenAddr)
	log.Printf("📦 MinIO endpoint : %s", cfg.MinioEndpoint)
	log.Printf("🗂  Bucket         : %s", cfg.MinioBucket)
	log.Printf("🔗 Public base URL : %s", cfg.MinioPublicBase)
	log.Printf("🛠  Admin UI       : http://localhost%s/admin/", cfg.ListenAddr)

	if err := http.ListenAndServe(cfg.ListenAddr, srv.routes()); err != nil {
		log.Fatal(err)
	}
}
