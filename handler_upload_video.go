package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading video", videoID, "by user", userID)

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Video couldn't be found", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "User not authorized to access video", nil)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Error parsing video", err)
		return
	}
	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		respondWithError(w, http.StatusBadRequest, "Missing Content-Type for video", nil)
	}

	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "unable to determine file type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "invalid file type", nil)
		return
	}

	// Create a temporary copy of the uploaded file locally
	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to create temp file location", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()
	written, err := io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to write video to disk at temp location", err)
		return
	}
	fmt.Println("User", userID, "wrote", written, "bytes to", tempFile)

	tempFile.Seek(0, io.SeekStart)

	extensions, err := mime.ExtensionsByType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "unable to determine file type", err)
		return
	}
	if len(extensions) == 0 {
		respondWithError(w, http.StatusBadRequest, "no file extension found for media type", nil)
		return
	}

	// Create the file key for AWS
	fileExtension := extensions[0]
	key := make([]byte, 32)
	_, err = rand.Read(key)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error randomizing key", err)
		return
	}
	rawFileKey := base64.RawURLEncoding.EncodeToString(key)
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to determine aspect ratio", err)
		return
	}
	aspectRatioSchema := ""
	switch aspectRatio {
	case "16:9":
		aspectRatioSchema = "landscape"
	case "9:16":
		aspectRatioSchema = "portrait"
	default:
		aspectRatioSchema = "other"
	}

	fileKey := fmt.Sprintf("%s/%s.%s", aspectRatioSchema, rawFileKey, fileExtension)
	processedVideoFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to process video for fast start", err)
		return
	}
	processedVideo, err := os.Open(processedVideoFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to read processed video file", err)
		return
	}
	defer os.Remove(processedVideoFilePath)
	defer processedVideo.Close()

	// Upload the video file to AWS S3 bucket
	s3PutParams := s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileKey,
		Body:        processedVideo,
		ContentType: &mediaType,
	}
	_, err = cfg.s3Client.PutObject(r.Context(), &s3PutParams)
	if err != nil {
		errorMessage := fmt.Sprintf("unable to write  file to s3 bucket: %s", cfg.s3Bucket)
		respondWithError(w, http.StatusBadRequest, errorMessage, err)
		return
	}

	// Write the videoURL to our database
	bucketKeyString := fmt.Sprintf("%v,%v", cfg.s3Bucket, fileKey)
	video.VideoURL = &bucketKeyString

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	signedVideo, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating video URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, signedVideo)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	fmt.Printf("filePath: %s \r\n", filePath)
	var buffer bytes.Buffer
	cmd.Stdout = &buffer

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffprobe error: %s", err)
	}

	var videoProps struct {
		Streams []struct {
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			AspectRatio string `json:"display_aspect_ratio"`
		} `json:"streams"`
	}

	if err := json.Unmarshal(buffer.Bytes(), &videoProps); err != nil {
		log.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	if len(videoProps.Streams) == 0 {
		return "", errors.New("no video streams found")
	}

	width := videoProps.Streams[0].Width
	height := videoProps.Streams[0].Height
	aspectRatio := videoProps.Streams[0].AspectRatio

	if aspectRatio != "" {
		fmt.Printf("Display Aspect Ratio: %v", aspectRatio)
		return aspectRatio, nil
	}

	if width*9 == height*16 {
		fmt.Printf("Width: %d, Height: %d, Ratio: %s \r\n", width*9, height*16, "16:9")
		return "16:9", nil
	} else if width*16 == height*9 {
		fmt.Printf("Width: %d, Height: %d, Ratio: %s \r\n", width*16, height*9, "9:16")
		return "9:16", nil
	} else {
		fmt.Printf("Width: %d, Height: %d, Ratio: %s \r\n", width*16, height*9, "Other")
		return "other", nil
	}
}

func processVideoForFastStart(filePath string) (string, error) {
	outputFilePath := filePath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilePath)
	var buffer bytes.Buffer
	cmd.Stdout = &buffer

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg error: %s", err)
	}
	return outputFilePath, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}

	url := strings.Split(*video.VideoURL, ",")
	if len(url) < 2 {
		return video, fmt.Errorf("invalid video URL format: expected bucket,key")
	}

	presignedURL, err := generatePresignedURL(cfg.s3Client, url[0], url[1], time.Hour)
	if err != nil {
		return video, fmt.Errorf("unable to generate presigned URL: %s", err)
	}

	video.VideoURL = &presignedURL
	return video, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	presignedUrl, err := presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL: %v", err)
	}
	return presignedUrl.URL, nil
}
