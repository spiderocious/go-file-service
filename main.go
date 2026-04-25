package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/google/uuid"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

var (
	uploadCount  atomic.Int64
	getFileCount atomic.Int64
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("no .env file found, using environment variables")
	}

	bucket := mustEnv("S3_BUCKET")
	endpoint := mustEnv("S3_ENDPOINT")
	region := mustEnv("S3_REGION")
	accessKey := mustEnv("S3_ACCESS_KEY_ID")
	secretKey := mustEnv("S3_SECRET_ACCESS_KEY")
	port := getEnv("PORT", "8080")

	presignClient := buildPresignClient(endpoint, region, accessKey, secretKey)

	r := gin.New()
	r.Use(gin.Logger())
	r.Use(gin.Recovery())

	r.Use(memStatsLogger())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	r.GET("/get-upload-uri", func(c *gin.Context) {
		prefix := c.Query("prefix")
		suffix := c.Query("suffix")

		if prefix != "" && len(prefix) != 5 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "prefix must be exactly 5 characters"})
			return
		}
		if suffix != "" && len(suffix) != 5 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "suffix must be exactly 5 characters"})
			return
		}

		id := uuid.New().String()
		key := id
		if prefix != "" {
			key = prefix + "-" + key
		}
		if suffix != "" {
			key = key + "-" + suffix
		}

		presignReq, err := presignClient.PresignPutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		}, s3.WithPresignExpires(15*time.Minute))
		if err != nil {
			log.Printf("[ERROR] get-upload-uri key=%s err=%v", key, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate upload URI"})
			return
		}

		count := uploadCount.Add(1)
		log.Printf("[UPLOAD-URI] key=%s total_upload_uris=%d", key, count)

		c.JSON(http.StatusOK, gin.H{
			"key":        key,
			"uri":        presignReq.URL,
			"expires_in": "15m",
		})
	})

	r.GET("/get-file-uri", func(c *gin.Context) {
		key := c.Query("key")
		if key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key is required"})
			return
		}

		presignReq, err := presignClient.PresignGetObject(context.Background(), &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		}, s3.WithPresignExpires(1*time.Hour))
		if err != nil {
			log.Printf("[ERROR] get-file-uri key=%s err=%v", key, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate file URI"})
			return
		}

		count := getFileCount.Add(1)
		log.Printf("[FILE-URI] key=%s total_file_uris=%d", key, count)

		c.JSON(http.StatusOK, gin.H{
			"uri": presignReq.URL,
			"expires_in": "1h",
		})
	})

	log.Printf("[STARTUP] file-service running on :%s | bucket=%s endpoint=%s", port, bucket, endpoint)
	if err := r.Run(":" + port); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func buildPresignClient(endpoint, region, accessKey, secretKey string) *s3.PresignClient {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	return s3.NewPresignClient(s3Client)
}

// memStatsLogger logs memory stats every 60 seconds and attaches alloc to each request log.
func memStatsLogger() gin.HandlerFunc {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		for range ticker.C {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			log.Printf("[MEMORY] alloc=%s sys=%s gc_cycles=%d goroutines=%d",
				formatBytes(m.Alloc),
				formatBytes(m.Sys),
				m.NumGC,
				runtime.NumGoroutine(),
			)
		}
	}()

	return func(c *gin.Context) {
		c.Next()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		log.Printf("[MEM/REQ] path=%s alloc=%s goroutines=%d",
			c.FullPath(),
			formatBytes(m.Alloc),
			runtime.NumGoroutine(),
		)
	}
}

func formatBytes(b uint64) string {
	switch {
	case b >= 1024*1024:
		return fmt.Sprintf("%.2fMB", float64(b)/1024/1024)
	case b >= 1024:
		return fmt.Sprintf("%.2fKB", float64(b)/1024)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing required env var: %s", key)
	}
	return v
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
