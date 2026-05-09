package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Config struct {
	Port           string
	WorkDir        string
	FFmpegBin      string
	WatermarkText  string
	WatermarkFont  string
	CallbackSecret string
	COSAppID       string
	COSBucket      string
	COSRegion      string
	COSSecretID    string
	COSSecretKey   string
	COSCDNHost     string
}

type JobRequest struct {
	JobID             string `json:"job_id"`
	SourceURL         string `json:"source_url"`
	BizType           string `json:"biz_type"`
	BizID             int64  `json:"biz_id"`
	WatermarkTemplate string `json:"watermark_template"`
	TargetFormat      string `json:"target_format"`
	TargetResolution  string `json:"target_resolution"`
	CallbackURL       string `json:"callback_url"`
}

type CallbackPayload struct {
	JobID            string  `json:"job_id"`
	Status           string  `json:"status"`
	ProcessedURL     string  `json:"processed_url,omitempty"`
	ThumbnailURL     string  `json:"thumbnail_url,omitempty"`
	Duration         float64 `json:"duration,omitempty"`
	Width            int     `json:"width,omitempty"`
	Height           int     `json:"height,omitempty"`
	ErrorMessage     string  `json:"error_message,omitempty"`
	WatermarkApplied bool    `json:"watermark_applied"`
	Compressed       bool    `json:"compressed"`
}

type Job struct {
	JobRequest
	Status           string    `json:"status"`
	ProcessedURL     string    `json:"processed_url,omitempty"`
	ThumbnailURL     string    `json:"thumbnail_url,omitempty"`
	ErrorMessage     string    `json:"error_message,omitempty"`
	Duration         float64   `json:"duration,omitempty"`
	Width            int       `json:"width,omitempty"`
	Height           int       `json:"height,omitempty"`
	WatermarkApplied bool      `json:"watermark_applied"`
	Compressed       bool      `json:"compressed"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type COSUploader struct {
	client     *s3.Client
	bucketName string
	publicHost string
	endpoint   string
}

type Server struct {
	cfg        Config
	httpClient *http.Client
	uploader   *COSUploader
	mu         sync.RWMutex
	jobs       map[string]*Job
}

const (
	downloadRetryAttempts = 3
	uploadRetryAttempts   = 3
	callbackRetryAttempts = 5
	retryBaseDelay        = 2 * time.Second
)

func main() {
	cfg := Config{
		Port:           getenv("PORT", "9096"),
		WorkDir:        getenv("WORK_DIR", "./data"),
		FFmpegBin:      getenv("FFMPEG_BIN", "ffmpeg"),
		WatermarkText:  getenv("WATERMARK_TEXT", "创意喵"),
		WatermarkFont:  getenv("WATERMARK_FONT", "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc"),
		CallbackSecret: getenv("CALLBACK_SECRET", getenv("VIDEO_PROCESSING_CALLBACK_SECRET", "")),
		COSAppID:       getenv("COS_APP_ID", ""),
		COSBucket:      getenv("COS_BUCKET", ""),
		COSRegion:      getenv("COS_REGION", ""),
		COSSecretID:    getenv("COS_SECRET_ID", ""),
		COSSecretKey:   getenv("COS_SECRET_KEY", ""),
		COSCDNHost:     strings.TrimRight(getenv("COS_CDN_HOST", ""), "/"),
	}

	if err := os.MkdirAll(filepath.Join(cfg.WorkDir, "tmp"), 0o755); err != nil {
		log.Fatalf("create tmp dir: %v", err)
	}

	uploader, err := newCOSUploader(cfg)
	if err != nil {
		log.Fatalf("init cos uploader: %v", err)
	}

	s := &Server{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 10 * time.Minute},
		uploader:   uploader,
		jobs:       map[string]*Job{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/jobs", s.handleJobs)
	mux.HandleFunc("/jobs/", s.handleJobByID)

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("miao_dataService listening on :%s", cfg.Port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func newCOSUploader(cfg Config) (*COSUploader, error) {
	if strings.TrimSpace(cfg.COSBucket) == "" {
		return nil, fmt.Errorf("COS_BUCKET is required")
	}
	if strings.TrimSpace(cfg.COSRegion) == "" {
		return nil, fmt.Errorf("COS_REGION is required")
	}
	if strings.TrimSpace(cfg.COSSecretID) == "" || strings.TrimSpace(cfg.COSSecretKey) == "" {
		return nil, fmt.Errorf("COS credentials are required")
	}

	bucketName := resolveCOSBucketName(cfg.COSBucket, cfg.COSAppID)
	if bucketName == "" {
		return nil, fmt.Errorf("invalid COS bucket/appid configuration")
	}
	endpoint := fmt.Sprintf("https://cos.%s.myqcloud.com", cfg.COSRegion)
	publicHost := cfg.COSCDNHost
	if publicHost == "" {
		publicHost = fmt.Sprintf("https://%s.cos.%s.myqcloud.com", bucketName, cfg.COSRegion)
	}

	resolver := aws.EndpointResolverWithOptionsFunc(
		func(service, region string, options ...interface{}) (aws.Endpoint, error) {
			return aws.Endpoint{
				URL:               endpoint,
				SigningRegion:     cfg.COSRegion,
				HostnameImmutable: false,
				Source:            aws.EndpointSourceCustom,
			}, nil
		},
	)

	awsCfg := aws.Config{
		Region:                      cfg.COSRegion,
		Credentials:                 credentials.NewStaticCredentialsProvider(cfg.COSSecretID, cfg.COSSecretKey, ""),
		EndpointResolverWithOptions: resolver,
		HTTPClient:                  &http.Client{Timeout: 10 * time.Minute},
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = false
		o.UseAccelerate = false
	})

	return &COSUploader{
		client:     client,
		bucketName: bucketName,
		publicHost: strings.TrimRight(publicHost, "/"),
		endpoint:   endpoint,
	}, nil
}

func resolveCOSBucketName(bucket, appID string) string {
	bucket = strings.TrimSpace(bucket)
	appID = strings.TrimSpace(appID)
	if bucket == "" {
		return ""
	}
	if strings.Contains(bucket, "-") {
		return bucket
	}
	if appID == "" {
		return bucket
	}
	return fmt.Sprintf("%s-%s", bucket, appID)
}

func (u *COSUploader) UploadFile(ctx context.Context, key, localPath, contentType string) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return "", err
	}

	_, err = u.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &u.bucketName,
		Key:           &key,
		Body:          f,
		ContentLength: aws.Int64(stat.Size()),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("put object: %w", err)
	}

	return u.PublicURL(key), nil
}

func (u *COSUploader) PublicURL(key string) string {
	return u.publicHost + "/" + strings.TrimLeft(key, "/")
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}

	if s.cfg.CallbackSecret != "" {
		signature := r.Header.Get("X-Miao-Signature")
		if !s.verifySignature(body, signature) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
			return
		}
	}

	var req JobRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if strings.TrimSpace(req.JobID) == "" || strings.TrimSpace(req.SourceURL) == "" || strings.TrimSpace(req.CallbackURL) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing required fields"})
		return
	}

	s.mu.Lock()
	if job, ok := s.jobs[req.JobID]; ok {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, job)
		return
	}
	job := &Job{
		JobRequest: req,
		Status:     "pending",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	s.jobs[req.JobID] = job
	s.mu.Unlock()

	go s.processJob(req.JobID)

	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleJobByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	jobID := strings.TrimPrefix(r.URL.Path, "/jobs/")
	if jobID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing job id"})
		return
	}
	s.mu.RLock()
	job, ok := s.jobs[jobID]
	s.mu.RUnlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) processJob(jobID string) {
	job, ok := s.getJob(jobID)
	if !ok {
		return
	}
	log.Printf("[video-processing] job=%s stage=start", jobID)
	s.updateJob(jobID, func(j *Job) {
		j.Status = "processing"
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	log.Printf("[video-processing] job=%s stage=download start", jobID)
	inputPath, err := s.downloadSource(ctx, job.SourceURL, job.JobID)
	if err != nil {
		s.failJob(jobID, fmt.Sprintf("download source failed: %v", err))
		return
	}
	log.Printf("[video-processing] job=%s stage=download done", jobID)
	defer os.Remove(inputPath)

	outputPath := filepath.Join(s.cfg.WorkDir, "tmp", job.JobID+"-output.mp4")
	thumbPath := filepath.Join(s.cfg.WorkDir, "tmp", job.JobID+"-thumb.jpg")
	defer os.Remove(outputPath)
	defer os.Remove(thumbPath)

	log.Printf("[video-processing] job=%s stage=ffmpeg start", jobID)
	if err := s.retryWithBackoff(ctx, jobID, "ffmpeg", 2, retryBaseDelay, func() error {
		return s.runFFmpeg(ctx, inputPath, outputPath, thumbPath, job.TargetResolution)
	}); err != nil {
		s.failJob(jobID, fmt.Sprintf("ffmpeg failed: %v", err))
		return
	}
	log.Printf("[video-processing] job=%s stage=ffmpeg done", jobID)

	videoKey := fmt.Sprintf("claim-processed/%d/%s.mp4", job.BizID, job.JobID)
	thumbKey := fmt.Sprintf("claim-processed/%d/%s.jpg", job.BizID, job.JobID)

	log.Printf("[video-processing] job=%s stage=upload_video start key=%s", jobID, videoKey)
	processedURL, err := s.uploadFileWithRetry(ctx, jobID, "upload_video", videoKey, outputPath, "video/mp4")
	if err != nil {
		s.failJob(jobID, fmt.Sprintf("upload processed video failed: %v", err))
		return
	}
	log.Printf("[video-processing] job=%s stage=upload_video done", jobID)

	log.Printf("[video-processing] job=%s stage=upload_thumbnail start key=%s", jobID, thumbKey)
	thumbnailURL, err := s.uploadFileWithRetry(ctx, jobID, "upload_thumbnail", thumbKey, thumbPath, "image/jpeg")
	if err != nil {
		s.failJob(jobID, fmt.Sprintf("upload thumbnail failed: %v", err))
		return
	}
	log.Printf("[video-processing] job=%s stage=upload_thumbnail done", jobID)

	payload := CallbackPayload{
		JobID:            job.JobID,
		Status:           "done",
		ProcessedURL:     processedURL,
		ThumbnailURL:     thumbnailURL,
		WatermarkApplied: true,
		Compressed:       true,
	}

	s.updateJob(jobID, func(j *Job) {
		j.Status = payload.Status
		j.ProcessedURL = payload.ProcessedURL
		j.ThumbnailURL = payload.ThumbnailURL
		j.WatermarkApplied = payload.WatermarkApplied
		j.Compressed = payload.Compressed
	})

	log.Printf("[video-processing] job=%s stage=callback start", jobID)
	if err := s.postCallbackWithRetry(payload, job.CallbackURL, jobID); err != nil {
		log.Printf("[video-processing] job=%s stage=callback warning: %v", jobID, err)
		return
	}
	log.Printf("[video-processing] job=%s stage=callback done", jobID)
}

func (s *Server) failJob(jobID, errMsg string) {
	job, ok := s.getJob(jobID)
	if !ok {
		return
	}
	log.Printf("[video-processing] job=%s failed: %s", jobID, errMsg)
	s.updateJob(jobID, func(j *Job) {
		j.Status = "failed"
		j.ErrorMessage = errMsg
	})
	_ = s.postCallbackWithRetry(CallbackPayload{
		JobID:        job.JobID,
		Status:       "failed",
		ErrorMessage: errMsg,
	}, job.CallbackURL, jobID)
}

func (s *Server) downloadSource(ctx context.Context, sourceURL, jobID string) (string, error) {
	var inputPath string
	err := s.retryWithBackoff(ctx, jobID, "download", downloadRetryAttempts, retryBaseDelay, func() error {
		var err error
		inputPath, err = s.downloadSourceOnce(ctx, sourceURL, jobID)
		return err
	})
	return inputPath, err
}

func (s *Server) downloadSourceOnce(ctx context.Context, sourceURL, jobID string) (inputPath string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &httpStatusError{stage: "download", code: resp.StatusCode}
	}

	inputPath = filepath.Join(s.cfg.WorkDir, "tmp", jobID+"-input"+sourceExt(sourceURL))
	f, err := os.Create(inputPath)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(inputPath)
		}
	}()
	if _, err = io.Copy(f, resp.Body); err != nil {
		return "", err
	}
	return inputPath, nil
}

func (s *Server) runFFmpeg(ctx context.Context, inputPath, outputPath, thumbPath, targetResolution string) error {
	if _, err := exec.LookPath(s.cfg.FFmpegBin); err != nil {
		return fmt.Errorf("ffmpeg not found: %w", err)
	}

	filter := buildVideoFilter(scaleWidthForResolution(targetResolution), s.cfg.WatermarkText, s.cfg.WatermarkFont)
	cmd := exec.CommandContext(ctx, s.cfg.FFmpegBin,
		"-y",
		"-i", inputPath,
		"-filter_complex", filter,
		"-map", "[v]",
		"-map", "0:a?",
		"-c:v", "libx264",
		"-profile:v", "high",
		"-pix_fmt", "yuv420p",
		"-preset", "veryfast",
		"-crf", "28",
		"-c:a", "aac",
		"-movflags", "+faststart",
		outputPath,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}

	thumbCmd := exec.CommandContext(ctx, s.cfg.FFmpegBin,
		"-y",
		"-i", outputPath,
		"-ss", "1",
		"-frames:v", "1",
		thumbPath,
	)
	if thumbOutput, err := thumbCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(thumbOutput)))
	}
	return nil
}

func buildVideoFilter(maxWidth int, watermarkText, watermarkFont string) string {
	base := fmt.Sprintf("[0:v]scale='if(gt(iw,%d),%d,iw)':-2,split=2[base][wm]", maxWidth, maxWidth)
	drawText := fmt.Sprintf("drawtext=text='%s':x=(w-tw)/2:y=(h-th)/2:fontsize=72:fontcolor=white:box=1:boxcolor=black@0.45:boxborderw=18", escapeFFmpegText(watermarkText))
	if font := strings.TrimSpace(watermarkFont); font != "" {
		drawText = fmt.Sprintf("drawtext=fontfile='%s':text='%s':x=(w-tw)/2:y=(h-th)/2:fontsize=72:fontcolor=white:box=1:boxcolor=black@0.45:boxborderw=18", escapeFFmpegText(font), escapeFFmpegText(watermarkText))
	}
	overlay := fmt.Sprintf("[wm]format=rgba,colorchannelmixer=aa=0,%s,rotate=PI/4:c=none:ow=rotw(iw):oh=roth(ih)[rotated];[base][rotated]overlay=(W-w)/2:(H-h)/2:format=auto,format=yuv420p[v]", drawText)
	return base + ";" + overlay
}

func scaleWidthForResolution(targetResolution string) int {
	switch strings.ToUpper(strings.TrimSpace(targetResolution)) {
	case "720P":
		return 720
	case "2K":
		return 1440
	case "4K":
		return 2160
	default:
		return 1080
	}
}

func sourceExt(sourceURL string) string {
	trimmed := strings.TrimSpace(sourceURL)
	if trimmed == "" {
		return ".mp4"
	}
	trimmed = strings.Split(trimmed, "?")[0]
	ext := strings.ToLower(filepath.Ext(trimmed))
	if ext == "" {
		return ".mp4"
	}
	return ext
}

func (s *Server) uploadFileWithRetry(ctx context.Context, jobID, stage, key, localPath, contentType string) (string, error) {
	var uploadedURL string
	err := s.retryWithBackoff(ctx, jobID, stage, uploadRetryAttempts, retryBaseDelay, func() error {
		var err error
		uploadedURL, err = s.uploader.UploadFile(ctx, key, localPath, contentType)
		return err
	})
	return uploadedURL, err
}

func (s *Server) postCallbackWithRetry(payload CallbackPayload, callbackURL, jobID string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.retryWithBackoff(context.Background(), jobID, "callback", callbackRetryAttempts, retryBaseDelay, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, callbackURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if s.cfg.CallbackSecret != "" {
			req.Header.Set("X-Miao-Signature", s.signBody(body))
		}
		resp, err := s.httpClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return &httpStatusError{stage: "callback", code: resp.StatusCode}
		}
		return nil
	})
}

func (s *Server) signBody(body []byte) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.CallbackSecret))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) verifySignature(body []byte, signature string) bool {
	if strings.TrimSpace(signature) == "" {
		return false
	}
	expected := s.signBody(body)
	return hmac.Equal([]byte(expected), []byte(strings.TrimSpace(signature)))
}

func (s *Server) getJob(jobID string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return nil, false
	}
	copyJob := *job
	return &copyJob, true
}

func (s *Server) updateJob(jobID string, mutate func(job *Job)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return
	}
	mutate(job)
	job.UpdatedAt = time.Now()
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func escapeFFmpegText(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", ":", "\\:", "'", "\\'", ",", "\\,")
	return replacer.Replace(value)
}

func (s *Server) retryWithBackoff(ctx context.Context, jobID, stage string, maxAttempts int, baseDelay time.Duration, fn func() error) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := fn(); err != nil {
			lastErr = err
			if attempt == maxAttempts || !isRetryableProcessingError(err) {
				return err
			}
			wait := baseDelay * time.Duration(1<<(attempt-1))
			log.Printf("[video-processing] job=%s stage=%s retry=%d/%d wait=%s err=%v", jobID, stage, attempt, maxAttempts, wait, err)
			if !sleepWithContext(ctx, wait) {
				if ctx != nil && ctx.Err() != nil {
					return ctx.Err()
				}
				return lastErr
			}
			continue
		}
		return nil
	}
	return lastErr
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if ctx == nil {
		time.Sleep(d)
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type httpStatusError struct {
	stage string
	code  int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s status %d", e.stage, e.code)
}

func isRetryableProcessingError(err error) bool {
	if err == nil {
		return false
	}

	var statusErr *httpStatusError
	if errors.As(err, &statusErr) {
		switch statusErr.code {
		case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusTooEarly:
			return true
		}
		return statusErr.code >= 500 && statusErr.code < 600
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() || netErr.Temporary() {
			return true
		}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout"),
		strings.Contains(msg, "temporarily unavailable"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "unexpected eof"),
		strings.Contains(msg, "eof"),
		strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "server error"),
		strings.Contains(msg, "connection refused"):
		return true
	default:
		return false
	}
}
