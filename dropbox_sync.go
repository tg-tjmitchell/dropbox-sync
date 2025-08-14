package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dropbox/dropbox-sdk-go-unofficial/v6/dropbox"
	"github.com/dropbox/dropbox-sdk-go-unofficial/v6/dropbox/files"
)

// Feature parity constants (mirroring Python notebook defaults)
const (
	largeFileThreshold = 145 * 1024 * 1024 // 145MB safe threshold under 150MB single-upload API limit
	chunkSize          = 8 * 1024 * 1024   // 8MB upload session chunk
	mtimeSkewSeconds   = 2                 // treat local newer if >2s newer than remote
)

// syncLocalFile represents a local file candidate for synchronization.
type syncLocalFile struct {
	RelPath  string
	FullPath string
	Size     int64
	MTime    time.Time
}

// syncRemoteFile holds the subset of remote metadata needed for decisions.
type syncRemoteFile struct {
	PathDisplay    string
	ClientModified time.Time
	Size           int64
}

type taskType int

const (
	taskUpload taskType = iota
	taskDelete
	taskDownload
)

type task struct {
	Kind       taskType
	Local      *syncLocalFile // for uploads
	RemotePath string
	Remote     *syncRemoteFile // for downloads
	Rel        string          // relative path for downloads/logging
}

// configFileModel models the JSON config file format.
type configFileModel struct {
	AccessToken  string  `json:"access_token"`
	LocalFolder  string  `json:"local_folder"`
	RemoteFolder string  `json:"remote_folder"`
	Delete       *bool   `json:"delete,omitempty"`
	Download     *bool   `json:"download,omitempty"`
	Mode         *string `json:"mode,omitempty"`
	Workers      *int    `json:"workers,omitempty"`
}

func main() {
	// --- Flags & Configuration ---
	var (
		flagConfig    = flag.String("config", "", "Path to JSON config file (used if flags/env missing)")
		flagToken     = flag.String("token", "", "Dropbox access token (overrides env & config)")
		flagLocal     = flag.String("local", os.Getenv("DBSYNC_LOCAL_FOLDER"), "Local folder (env DBSYNC_LOCAL_FOLDER / LOCAL_FOLDER)")
		flagRemote    = flag.String("remote", os.Getenv("DBSYNC_REMOTE_FOLDER"), "Dropbox folder starting with '/' (env DBSYNC_REMOTE_FOLDER / DROPBOX_FOLDER)")
		flagDelete    = flag.Bool("delete", false, "Delete remote files not present locally")
		flagDownload  = flag.Bool("download", false, "Download remote files missing locally or where remote copy is newer")
		flagMode      = flag.String("mode", "", "Sync mode: upload|download|two-way|mirror (overrides --delete/--download)")
		flagWorkers   = flag.Int("workers", minInt(8, runtime.NumCPU()*2), "Parallel worker count")
		flagDryRun    = flag.Bool("dry-run", false, "Show actions without executing")
		flagVerbose   = flag.Bool("v", false, "Verbose logging")
		flagNoProgBar = flag.Bool("no-progress", false, "Disable progress bar output (auto disabled with -v)")
		flagBarWidth  = flag.Int("bar-width", 40, "Progress bar width (characters)")
	)
	flag.Parse()
	// Resolve configuration order: flags > env vars > config file
	token := firstNonEmpty(*flagToken,
		os.Getenv("DBSYNC_ACCESS_TOKEN"),
		os.Getenv("DROPBOX_ACCESS_TOKEN"))

	// If any core pieces missing, attempt config file.
	if (token == "" || *flagLocal == "" || *flagRemote == "") && *flagConfig != "" {
		cfg, err := loadConfigFile(*flagConfig)
		if err != nil {
			log.Fatalf("failed to load config file: %v", err)
		}
		if token == "" {
			token = cfg.AccessToken
		}
		if *flagLocal == "" && cfg.LocalFolder != "" {
			*flagLocal = cfg.LocalFolder
		}
		if *flagRemote == "" && cfg.RemoteFolder != "" {
			*flagRemote = cfg.RemoteFolder
		}
		if cfg.Delete != nil && !flagPassed("delete") {
			*flagDelete = *cfg.Delete
		}
		if cfg.Download != nil && !flagPassed("download") {
			*flagDownload = *cfg.Download
		}
		if cfg.Mode != nil && !flagPassed("mode") {
			*flagMode = *cfg.Mode
		}
		if cfg.Workers != nil && !flagPassed("workers") {
			*flagWorkers = *cfg.Workers
		}
	}
	// If still not found, attempt default config file name.
	if (token == "" || *flagLocal == "" || *flagRemote == "") && *flagConfig == "" {
		if _, err := os.Stat("dropbox-sync.json"); err == nil {
			if cfg, err2 := loadConfigFile("dropbox-sync.json"); err2 == nil {
				if token == "" {
					token = cfg.AccessToken
				}
				if *flagLocal == "" && cfg.LocalFolder != "" {
					*flagLocal = cfg.LocalFolder
				}
				if *flagRemote == "" && cfg.RemoteFolder != "" {
					*flagRemote = cfg.RemoteFolder
				}
				if cfg.Delete != nil && !flagPassed("delete") {
					*flagDelete = *cfg.Delete
				}
				if cfg.Download != nil && !flagPassed("download") {
					*flagDownload = *cfg.Download
				}
				if cfg.Mode != nil && !flagPassed("mode") {
					*flagMode = *cfg.Mode
				}
				if cfg.Workers != nil && !flagPassed("workers") {
					*flagWorkers = *cfg.Workers
				}
			}
		}
	}
	if token == "" {
		log.Fatal("Dropbox access token not provided (flag --token, env DBSYNC_ACCESS_TOKEN / DROPBOX_ACCESS_TOKEN, or config file)")
	}
	if *flagLocal == "" {
		log.Fatal("Local folder not provided (flag --local, env DBSYNC_LOCAL_FOLDER / LOCAL_FOLDER, or config file)")
	}
	if *flagRemote == "" {
		log.Fatal("Remote folder not provided (flag --remote, env DBSYNC_REMOTE_FOLDER / DROPBOX_FOLDER, or config file)")
	}
	if !strings.HasPrefix(*flagRemote, "/") {
		log.Fatal("Remote Dropbox folder must start with '/'")
	}

	config := dropbox.Config{Token: token}
	client := files.New(config)

	start := time.Now()
	fmt.Println("Listing local files...")
	localFiles := collectLocalEntries(*flagLocal)
	fmt.Printf("Found %d local files.\n", len(localFiles))

	fmt.Println("Listing remote files (with pagination)...")
	remoteFiles := collectRemoteEntries(client, *flagRemote)
	fmt.Printf("Found %d remote files.\n", len(remoteFiles))

	// Apply mode (if provided) to normalize delete/download flags and decide if uploads are skipped
	skipUploads := false
	if *flagMode != "" {
		switch strings.ToLower(*flagMode) {
		case "upload":
			*flagDownload = false
			*flagDelete = false
		case "download":
			*flagDownload = true
			*flagDelete = false
			skipUploads = true
		case "two-way", "twoway", "two_way":
			*flagDownload = true
			*flagDelete = false
		case "mirror":
			*flagDownload = true
			*flagDelete = true
		default:
			log.Fatalf("invalid --mode value: %s", *flagMode)
		}
	}

	// Determine operations
	uploads := []*syncLocalFile{}
	deletes := []string{}
	downloads := []string{}

	if !skipUploads {
		for _, lf := range localFiles { // Upload new or modified
			rf, exists := remoteFiles[lf.RelPath]
			if !exists {
				uploads = append(uploads, lf)
				continue
			}
			remoteTime := rf.ClientModified
			if lf.MTime.Sub(remoteTime).Seconds() > mtimeSkewSeconds { // local newer
				uploads = append(uploads, lf)
			}
		}
	}

	if *flagDelete {
		for rel := range remoteFiles {
			if _, exists := localFiles[rel]; !exists {
				deletes = append(deletes, rel)
			}
		}
	}

	if *flagDownload {
		for rel, rf := range remoteFiles {
			lf, exists := localFiles[rel]
			if !exists { // missing locally
				downloads = append(downloads, rel)
				continue
			}
			// remote newer than local
			if rf.ClientModified.Sub(lf.MTime).Seconds() > mtimeSkewSeconds {
				// remove any upload scheduled for this file
				pruned := uploads[:0]
				for _, u := range uploads {
					if u.RelPath != rel {
						pruned = append(pruned, u)
					}
				}
				uploads = pruned
				downloads = append(downloads, rel)
			}
		}
	}

	if skipUploads {
		fmt.Printf("Uploads disabled by mode (%s)\n", *flagMode)
	} else {
		fmt.Printf("Planned uploads/updates: %d\n", len(uploads))
	}
	if *flagDelete {
		fmt.Printf("Planned deletions: %d\n", len(deletes))
	} else {
		fmt.Println("Deletions disabled (use --delete to enable)")
	}
	if *flagDownload {
		fmt.Printf("Planned downloads: %d\n", len(downloads))
	} else {
		fmt.Println("Downloads disabled (use --download to enable)")
	}
	if *flagDryRun {
		fmt.Println("--- DRY RUN ---")
		for _, u := range uploads {
			fmt.Printf("UPLOAD %s -> %s/%s\n", u.FullPath, *flagRemote, u.RelPath)
		}
		for _, d := range deletes {
			fmt.Printf("DELETE %s/%s\n", *flagRemote, d)
		}
		for _, dl := range downloads {
			rf := remoteFiles[dl]
			fmt.Printf("DOWNLOAD %s/%s -> %s\n", *flagRemote, dl, filepath.Join(*flagLocal, filepath.FromSlash(dl)))
			if rf != nil {
				fmt.Printf("  (remote mtime: %s size: %s)\n", rf.ClientModified.Format(time.RFC3339), humanBytes(rf.Size))
			}
		}
		fmt.Println("Dry run complete.")
		return
	}

	// Build task list
	totalTasks := len(uploads) + len(deletes) + len(downloads)
	tasks := make(chan task, totalTasks)
	var wg sync.WaitGroup
	var mu sync.Mutex
	errorsFound := []error{}
	var completed int64
	var bytesDone int64
	var totalBytes int64
	for _, u := range uploads {
		totalBytes += u.Size
	}
	for _, rel := range downloads {
		if rf, ok := remoteFiles[rel]; ok {
			totalBytes += rf.Size
		}
	}

	showProgress := !*flagNoProgBar && !*flagVerbose && totalTasks > 0
	doneCh := make(chan struct{})
	if showProgress {
		go func(total int, totalBytes int64, width int) {
			startBar := time.Now()
			ticker := time.NewTicker(120 * time.Millisecond)
			defer ticker.Stop()
			render := func(final bool) {
				comp := atomic.LoadInt64(&completed)
				pct := float64(comp) / float64(total)
				filled := int(pct * float64(width))
				if filled > width {
					filled = width
				}
				bar := strings.Repeat("#", filled) + strings.Repeat("-", width-filled)
				elapsed := time.Since(startBar)
				rate := float64(comp) / elapsed.Seconds()
				bytesNow := atomic.LoadInt64(&bytesDone)
				var etaStr string
				if comp > 0 && comp < int64(total) {
					remaining := float64(int64(total)-comp) / rate
					etaStr = formatDuration(time.Duration(remaining * float64(time.Second)))
				} else {
					etaStr = "0s"
				}
				fmt.Printf("\r[%s] %5.1f%% %d/%d | %s/%s | %.2f f/s | ETA %s Elapsed %s", bar, pct*100, comp, total, humanBytes(bytesNow), humanBytes(totalBytes), rate, etaStr, formatDuration(elapsed))
				if final {
					fmt.Println()
				}
			}
			for {
				select {
				case <-doneCh:
					render(true)
					return
				case <-ticker.C:
					render(false)
				}
			}
		}(totalTasks, totalBytes, *flagBarWidth)
	}

	worker := func(id int) {
		defer wg.Done()
		for t := range tasks {
			switch t.Kind {
			case taskUpload:
				if *flagVerbose {
					fmt.Printf("[worker %d] uploading %s\n", id, t.Local.RelPath)
				}
				if err := uploadFile(client, *flagRemote, t.Local, func(delta int64) {
					if delta > 0 {
						atomic.AddInt64(&bytesDone, delta)
					}
				}); err != nil {
					mu.Lock()
					errorsFound = append(errorsFound, fmt.Errorf("upload %s: %w", t.Local.RelPath, err))
					mu.Unlock()
				}
			case taskDelete:
				if *flagVerbose {
					fmt.Printf("[worker %d] deleting %s\n", id, t.RemotePath)
				}
				if err := deleteFile(client, t.RemotePath); err != nil {
					mu.Lock()
					errorsFound = append(errorsFound, fmt.Errorf("delete %s: %w", t.RemotePath, err))
					mu.Unlock()
				}
			case taskDownload:
				if *flagVerbose {
					fmt.Printf("[worker %d] downloading %s\n", id, t.Rel)
				}
				if err := downloadFile(client, t.RemotePath, *flagLocal, t.Rel, t.Remote, func(delta int64) {
					if delta > 0 {
						atomic.AddInt64(&bytesDone, delta)
					}
				}); err != nil {
					mu.Lock()
					errorsFound = append(errorsFound, fmt.Errorf("download %s: %w", t.Rel, err))
					mu.Unlock()
				}
			}
			if showProgress {
				atomic.AddInt64(&completed, 1)
			}
		}
	}

	for i := 0; i < *flagWorkers; i++ {
		wg.Add(1)
		go worker(i + 1)
	}
	for _, lf := range uploads {
		tasks <- task{Kind: taskUpload, Local: lf}
	}
	for _, rel := range deletes {
		rp := joinDropboxPath(*flagRemote, rel)
		tasks <- task{Kind: taskDelete, RemotePath: rp}
	}
	for _, rel := range downloads {
		rp := joinDropboxPath(*flagRemote, rel)
		tasks <- task{Kind: taskDownload, RemotePath: rp, Remote: remoteFiles[rel], Rel: rel}
	}
	close(tasks)
	wg.Wait()
	if showProgress {
		close(doneCh)
	}

	if len(errorsFound) > 0 {
		fmt.Printf("Completed with %d errors:\n", len(errorsFound))
		for _, e := range errorsFound {
			fmt.Println(" -", e)
		}
	} else {
		fmt.Println("All operations completed successfully.")
	}

	fmt.Printf("Total time: %s\n", time.Since(start).Round(time.Second))
	fmt.Println("--- Synchronization Complete ---")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// collectLocalEntries walks the local tree.
func collectLocalEntries(root string) map[string]*syncLocalFile {
	out := make(map[string]*syncLocalFile)
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		out[rel] = &syncLocalFile{RelPath: rel, FullPath: path, Size: info.Size(), MTime: info.ModTime().UTC()}
		return nil
	})
	return out
}

// collectRemoteEntries retrieves remote recursive listing.
func collectRemoteEntries(client files.Client, root string) map[string]*syncRemoteFile {
	out := make(map[string]*syncRemoteFile)
	arg := files.NewListFolderArg(root)
	arg.Recursive = true
	res, err := client.ListFolder(arg)
	if err != nil {
		fmt.Println("Warning: remote listing failed (treating as empty):", err)
		return out
	}
	process := func(entries []files.IsMetadata) {
		for _, entry := range entries {
			switch e := entry.(type) {
			case *files.FileMetadata:
				rel := strings.TrimPrefix(e.PathDisplay, root)
				rel = strings.TrimPrefix(rel, "/")
				if rel == "" {
					continue
				}
				out[rel] = &syncRemoteFile{PathDisplay: e.PathDisplay, ClientModified: e.ClientModified.UTC(), Size: int64(e.Size)}
			}
		}
	}
	process(res.Entries)
	for res.HasMore {
		res, err = client.ListFolderContinue(files.NewListFolderContinueArg(res.Cursor))
		if err != nil {
			fmt.Println("Error during pagination:", err)
			break
		}
		process(res.Entries)
	}
	return out
}

// loadConfigFile parses JSON config file.
func loadConfigFile(path string) (*configFileModel, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg configFileModel
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// flagPassed reports whether a flag was explicitly set.
func flagPassed(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func joinDropboxPath(root, rel string) string {
	root = strings.TrimRight(root, "/")
	if rel == "" {
		return root
	}
	return root + "/" + strings.ReplaceAll(rel, "\\", "/")
}

// uploadFile handles small and large uploads, preserving mtime
func uploadFile(client files.Client, remoteRoot string, lf *syncLocalFile, progress func(delta int64)) error {
	remotePath := joinDropboxPath(remoteRoot, lf.RelPath)
	f, err := os.Open(lf.FullPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// Prepare commit info
	upArg := files.NewUploadArg(remotePath)
	upArg.ClientModified = &lf.MTime
	// Overwrite mode:
	if upArg.Mode != nil {
		upArg.Mode.Tag = "overwrite"
	} else {
		upArg.Mode = &files.WriteMode{Tagged: dropbox.Tagged{Tag: "overwrite"}}
	}
	upArg.Mute = true

	if lf.Size <= largeFileThreshold {
		// Wrap reader to count bytes (single increment at end if small)
		data, errRead := io.ReadAll(f)
		if errRead != nil {
			return errRead
		}
		_, err = client.Upload(upArg, bytes.NewReader(data))
		if err == nil && progress != nil {
			progress(lf.Size)
		}
		return err
	}
	// Large file: manual session
	// Start session
	firstChunk := make([]byte, minInt(int(chunkSize), int(lf.Size)))
	n, err := io.ReadFull(f, firstChunk)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("read first chunk: %w", err)
	}
	startArg := files.NewUploadSessionStartArg()
	startRes, err := client.UploadSessionStart(startArg, bytes.NewReader(firstChunk[:n]))
	if err != nil {
		return fmt.Errorf("session start: %w", err)
	}
	if progress != nil {
		progress(int64(n))
	}

	offset := int64(n)
	for offset < lf.Size {
		remaining := lf.Size - offset
		thisChunk := int64(chunkSize)
		if remaining < thisChunk {
			thisChunk = remaining
		}
		buf := make([]byte, thisChunk)
		rn, err := io.ReadFull(f, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("read chunk: %w", err)
		}
		offset += int64(rn)
		cursor := files.NewUploadSessionCursor(startRes.SessionId, uint64(offset-int64(rn)))
		if offset < lf.Size { // append
			appendArg := files.NewUploadSessionAppendArg(cursor)
			err = client.UploadSessionAppendV2(appendArg, bytes.NewReader(buf[:rn]))
			if err != nil {
				return fmt.Errorf("session append: %w", err)
			}
			if progress != nil {
				progress(int64(rn))
			}
		} else { // finish
			commitInfo := files.NewCommitInfo(remotePath)
			commitInfo.ClientModified = &lf.MTime
			commitInfo.Mode = &files.WriteMode{Tagged: dropbox.Tagged{Tag: "overwrite"}}
			finishArg := files.NewUploadSessionFinishArg(cursor, commitInfo)
			_, err = client.UploadSessionFinish(finishArg, bytes.NewReader(buf[:rn]))
			if err != nil {
				return fmt.Errorf("session finish: %w", err)
			}
			if progress != nil {
				progress(int64(rn))
			}
		}
	}
	return nil
}

// downloadFile downloads a remote file to the local root preserving modified time.
func downloadFile(client files.Client, remotePath string, localRoot string, rel string, rf *syncRemoteFile, progress func(delta int64)) error {
	// Use Download API
	arg := files.NewDownloadArg(remotePath)
	res, content, err := client.Download(arg)
	if err != nil {
		return err
	}
	defer content.Close()
	// Ensure destination directory exists
	localPath := filepath.Join(localRoot, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	// Stream copy in chunks to report progress
	buf := make([]byte, 64*1024)
	var written int64
	for {
		n, rerr := content.Read(buf)
		if n > 0 {
			wn, werr := f.Write(buf[:n])
			written += int64(wn)
			if progress != nil {
				progress(int64(wn))
			}
			if werr != nil {
				return werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return rerr
		}
	}
	// Set mod time to remote client modified (prefer rf if provided, else res)
	var modTime time.Time
	if rf != nil {
		modTime = rf.ClientModified
	} else if res != nil {
		modTime = res.ClientModified
	} else {
		modTime = time.Now().UTC()
	}
	if !modTime.IsZero() {
		_ = os.Chtimes(localPath, time.Now(), modTime)
	}
	return nil
}

// deleteFile deletes a single remote path
func deleteFile(client files.Client, dropboxPath string) error {
	_, err := client.DeleteV2(files.NewDeleteArg(dropboxPath))
	return err
}

// bytesReader returns an io.ReadCloser for a byte slice without copying
// humanBytes converts byte counts to human-readable form
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// formatDuration shortens durations (e.g., 1h2m3s)
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return d.String()
	}
	hours := d / time.Hour
	d -= hours * time.Hour
	mins := d / time.Minute
	d -= mins * time.Minute
	secs := d / time.Second
	if hours > 0 {
		return fmt.Sprintf("%dh%dm%ds", hours, mins, secs)
	}
	if mins > 0 {
		return fmt.Sprintf("%dm%ds", mins, secs)
	}
	return fmt.Sprintf("%ds", secs)
}
