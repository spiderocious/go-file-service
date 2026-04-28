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
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
)

const (
	fileURIExpiry   = 1 * time.Hour
	cacheURIExpiry  = 50 * time.Minute
	cacheKeyPrefix  = "file-uri:"
)

var (
	uploadCount    atomic.Int64
	getFileCount   atomic.Int64
	cacheHitCount  atomic.Int64
	cacheMissCount atomic.Int64
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
	redisClient := buildRedisClient()

	if redisClient != nil {
		log.Println("[STARTUP] Redis cache enabled")
	} else {
		log.Println("[STARTUP] Redis not configured, caching disabled")
	}

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
		ext := c.Query("ext")

		if prefix != "" && len(prefix) != 5 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "prefix must be exactly 5 characters"})
			return
		}
		if suffix != "" && len(suffix) != 5 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "suffix must be exactly 5 characters"})
			return
		}
		if ext != "" {
			for _, ch := range ext {
				if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')) {
					c.JSON(http.StatusBadRequest, gin.H{"error": "ext must contain only alphanumeric characters (e.g. jpg, png, mp4)"})
					return
				}
			}
		}

		id := uuid.New().String()
		key := id
		if prefix != "" {
			key = prefix + "-" + key
		}
		if suffix != "" {
			key = key + "-" + suffix
		}
		if ext != "" {
			key = key + "." + ext
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

		if redisClient != nil {
			if cached, err := redisClient.Get(c, cacheKeyPrefix+key).Result(); err == nil {
				hits := cacheHitCount.Add(1)
				count := getFileCount.Add(1)
				log.Printf("[FILE-URI] key=%s cache=HIT total_file_uris=%d total_cache_hits=%d", key, count, hits)
				c.JSON(http.StatusOK, gin.H{"uri": cached, "expires_in": "1h", "cached": true})
				return
			}
		}

		presignReq, err := presignClient.PresignGetObject(context.Background(), &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		}, s3.WithPresignExpires(fileURIExpiry))
		if err != nil {
			log.Printf("[ERROR] get-file-uri key=%s err=%v", key, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate file URI"})
			return
		}

		if redisClient != nil {
			if err := redisClient.Set(c, cacheKeyPrefix+key, presignReq.URL, cacheURIExpiry).Err(); err != nil {
				log.Printf("[CACHE] failed to store key=%s err=%v", key, err)
			}
		}

		misses := cacheMissCount.Add(1)
		count := getFileCount.Add(1)
		log.Printf("[FILE-URI] key=%s cache=MISS total_file_uris=%d total_cache_misses=%d", key, count, misses)

		c.JSON(http.StatusOK, gin.H{
			"uri":        presignReq.URL,
			"expires_in": "1h",
			"cached":     false,
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

// buildRedisClient returns nil if REDIS_URL is not set.
func buildRedisClient() *redis.Client {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		return nil
	}

	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("[REDIS] invalid REDIS_URL: %v", err)
	}

	client := redis.NewClient(opt)
	if err := client.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("[REDIS] connection failed: %v", err)
	}

	return client
}

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
