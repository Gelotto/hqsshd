package session

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gelotto/hqsshd/internal/logging"
)

const (
	logBufferSize  = 4 * 1024        // 4KB buffer before flush
	flushInterval  = 100 * time.Millisecond
	logFileMode    = 0600 // Owner read/write only
	logDirMode     = 0700 // Owner full access only
)

// SessionLogger handles persistent logging of session output to disk.
// It uses buffered writes with periodic flushing for performance,
// and compresses logs to gzip on close.
type SessionLogger struct {
	sessionID string
	logDir    string
	file      *os.File
	buffer    *bufio.Writer
	mu        sync.Mutex

	// Flush timer for periodic flushing
	flushTimer *time.Timer

	// Track state
	closed       bool
	closeOnce    sync.Once // Ensures Close() is idempotent and thread-safe
	bytesWritten int64
}

// NewSessionLogger creates a new session logger that writes to:
// {logDir}/{sessionId}.log
func NewSessionLogger(sessionID, logDir string) (*SessionLogger, error) {
	// Ensure log directory exists
	if err := os.MkdirAll(logDir, logDirMode); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	logPath := filepath.Join(logDir, sessionID+".log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, logFileMode)
	if err != nil {
		return nil, fmt.Errorf("failed to create log file: %w", err)
	}

	l := &SessionLogger{
		sessionID: sessionID,
		logDir:    logDir,
		file:      file,
		buffer:    bufio.NewWriterSize(file, logBufferSize),
	}

	// Start periodic flush timer
	l.startFlushTimer()

	logging.Debug("session logger created",
		"session_id", sessionID,
		"log_path", logPath,
	)

	return l, nil
}

// Write writes data to the log buffer (thread-safe).
// Data is buffered and flushed periodically or when buffer is full.
func (l *SessionLogger) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return 0, fmt.Errorf("logger is closed")
	}

	n, err := l.buffer.Write(data)
	if err != nil {
		return n, err
	}

	l.bytesWritten += int64(n)

	// Flush if buffer is getting full
	if l.buffer.Available() < logBufferSize/4 {
		if flushErr := l.buffer.Flush(); flushErr != nil {
			logging.Warn("failed to flush session log",
				"session_id", l.sessionID,
				"error", flushErr,
			)
		}
	}

	return n, nil
}

// Flush forces a flush of the buffer to disk (thread-safe).
func (l *SessionLogger) Flush() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return nil
	}

	if err := l.buffer.Flush(); err != nil {
		return fmt.Errorf("failed to flush buffer: %w", err)
	}

	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("failed to sync file: %w", err)
	}

	return nil
}

// Close flushes remaining data, closes the file, and compresses to .gz.
// Safe to call multiple times - subsequent calls are no-ops.
func (l *SessionLogger) Close() error {
	l.closeOnce.Do(func() {
		// Mark as closed under lock to stop new writes and periodic flushes
		l.mu.Lock()
		l.closed = true

		// Stop flush timer while holding lock to prevent race with periodicFlush
		if l.flushTimer != nil {
			l.flushTimer.Stop()
		}

		// Flush any remaining data
		if err := l.buffer.Flush(); err != nil {
			logging.Warn("failed to flush buffer on close",
				"session_id", l.sessionID,
				"error", err,
			)
		}

		bytesWritten := l.bytesWritten
		l.mu.Unlock()

		// Close the file (outside lock - no concurrent access after closed=true)
		if err := l.file.Close(); err != nil {
			logging.Warn("failed to close log file",
				"session_id", l.sessionID,
				"error", err,
			)
		}

		// Compress the log file
		if err := l.compressLog(); err != nil {
			logging.Warn("failed to compress log file",
				"session_id", l.sessionID,
				"error", err,
			)
			// Don't return error - uncompressed log is still valid
		}

		logging.Info("session logger closed",
			"session_id", l.sessionID,
			"bytes_written", bytesWritten,
		)
	})

	return nil
}

// LogPath returns the current log file path (without .gz extension)
func (l *SessionLogger) LogPath() string {
	return filepath.Join(l.logDir, l.sessionID+".log")
}

// CompressedLogPath returns the compressed log file path
func (l *SessionLogger) CompressedLogPath() string {
	return filepath.Join(l.logDir, l.sessionID+".log.gz")
}

// BytesWritten returns the total bytes written to the log
func (l *SessionLogger) BytesWritten() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bytesWritten
}

// startFlushTimer starts the periodic flush timer
func (l *SessionLogger) startFlushTimer() {
	l.flushTimer = time.AfterFunc(flushInterval, func() {
		l.periodicFlush()
	})
}

// periodicFlush is called by the timer to flush periodically.
// Holds lock for entire operation to prevent race with Close().
func (l *SessionLogger) periodicFlush() {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Check if closed - if so, don't flush or reschedule
	if l.closed {
		return
	}

	// Flush buffered data
	if l.buffer.Buffered() > 0 {
		if err := l.buffer.Flush(); err != nil {
			logging.Warn("periodic flush failed",
				"session_id", l.sessionID,
				"error", err,
			)
		}
	}

	// Schedule next flush (still under lock, so Close() can't race)
	l.flushTimer.Reset(flushInterval)
}

// compressLog compresses the log file to .gz and removes the original
func (l *SessionLogger) compressLog() error {
	srcPath := l.LogPath()
	dstPath := l.CompressedLogPath()

	// Open source file
	srcFile, err := os.Open(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No log to compress
		}
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer srcFile.Close()

	// Get source file size for logging
	var srcSize int64
	if srcInfo, err := srcFile.Stat(); err == nil {
		srcSize = srcInfo.Size()
	}

	// Create destination file
	dstFile, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, logFileMode)
	if err != nil {
		return fmt.Errorf("failed to create compressed file: %w", err)
	}

	// Create gzip writer
	gzWriter := gzip.NewWriter(dstFile)

	// Copy data through gzip
	if _, err := io.Copy(gzWriter, srcFile); err != nil {
		gzWriter.Close()
		dstFile.Close()
		os.Remove(dstPath) // Clean up partial file
		return fmt.Errorf("failed to compress: %w", err)
	}

	// Close gzip writer (flushes remaining data)
	if err := gzWriter.Close(); err != nil {
		dstFile.Close()
		os.Remove(dstPath)
		return fmt.Errorf("failed to close gzip writer: %w", err)
	}

	// Close destination file
	closeErr := dstFile.Close()

	// Get compressed size for logging (before removing source)
	var dstSize int64
	if dstInfo, err := os.Stat(dstPath); err == nil {
		dstSize = dstInfo.Size()
	}

	// Remove original file - do this even if dstFile.Close() failed
	// because the compressed data is already written and flushed
	if err := os.Remove(srcPath); err != nil {
		logging.Warn("failed to remove original log file",
			"session_id", l.sessionID,
			"path", srcPath,
			"error", err,
		)
	}

	// Return close error if any (after cleanup)
	if closeErr != nil {
		return fmt.Errorf("failed to close compressed file: %w", closeErr)
	}

	// Log compression result (guard against division by zero for empty files)
	if srcSize > 0 {
		logging.Info("session log compressed",
			"session_id", l.sessionID,
			"original_size", srcSize,
			"compressed_size", dstSize,
			"compression_ratio", fmt.Sprintf("%.1f%%", float64(dstSize)/float64(srcSize)*100),
		)
	} else {
		logging.Info("session log compressed",
			"session_id", l.sessionID,
			"original_size", srcSize,
			"compressed_size", dstSize,
		)
	}

	return nil
}

// ReadLog reads the log file for a session (handles both .log and .log.gz)
// Returns an io.ReadCloser that must be closed by the caller.
func ReadLog(logDir, sessionID string) (io.ReadCloser, error) {
	// Try compressed file first
	gzPath := filepath.Join(logDir, sessionID+".log.gz")
	if _, err := os.Stat(gzPath); err == nil {
		file, err := os.Open(gzPath)
		if err != nil {
			return nil, fmt.Errorf("failed to open compressed log: %w", err)
		}

		gzReader, err := gzip.NewReader(file)
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}

		return &gzipReadCloser{gzReader: gzReader, file: file}, nil
	}

	// Try uncompressed file
	logPath := filepath.Join(logDir, sessionID+".log")
	file, err := os.Open(logPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open log: %w", err)
	}

	return file, nil
}

// GetLogSize returns the size of the log file (compressed or uncompressed)
func GetLogSize(logDir, sessionID string) (int64, error) {
	// Try compressed file first
	gzPath := filepath.Join(logDir, sessionID+".log.gz")
	if info, err := os.Stat(gzPath); err == nil {
		return info.Size(), nil
	}

	// Try uncompressed file
	logPath := filepath.Join(logDir, sessionID+".log")
	if info, err := os.Stat(logPath); err == nil {
		return info.Size(), nil
	}

	return 0, fmt.Errorf("log file not found")
}

// DeleteLog removes the log file (both .log and .log.gz if they exist)
func DeleteLog(logDir, sessionID string) error {
	gzPath := filepath.Join(logDir, sessionID+".log.gz")
	logPath := filepath.Join(logDir, sessionID+".log")

	var lastErr error

	if err := os.Remove(gzPath); err != nil && !os.IsNotExist(err) {
		lastErr = err
	}

	if err := os.Remove(logPath); err != nil && !os.IsNotExist(err) {
		lastErr = err
	}

	return lastErr
}

// gzipReadCloser wraps a gzip.Reader to close both the reader and underlying file
type gzipReadCloser struct {
	gzReader *gzip.Reader
	file     *os.File
}

func (g *gzipReadCloser) Read(p []byte) (int, error) {
	return g.gzReader.Read(p)
}

func (g *gzipReadCloser) Close() error {
	gzErr := g.gzReader.Close()
	fileErr := g.file.Close()
	if gzErr != nil {
		return gzErr
	}
	return fileErr
}
