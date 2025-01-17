package cmd

import (
    "github.com/cole-brown/transcoder-go/config"
    "github.com/cole-brown/transcoder-go/models"
    "github.com/cole-brown/transcoder-go/notifications"
    "github.com/cole-brown/transcoder-go/transcoder"
    "github.com/cole-brown/transcoder-go/utils"
	"github.com/bmatcuk/doublestar/v4"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"io/ioutil"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// TODO Make Configurable
const outputFileExtension = ".mkv"

var terminated bool

var LogLevel string
var ForceColors bool

var rootCmd = &cobra.Command{
	Use: "transcoder [flags] <path> ...",

	Short: "transcoder is an opinionated wrapper around ffmpeg",
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		level, err := log.ParseLevel(LogLevel)

		if err != nil {
			panic(err)
		}

		log.SetFormatter(&log.TextFormatter{
			ForceColors: ForceColors,
		})
		log.SetOutput(os.Stdout)
		log.SetLevel(level)

		config.InitializeConfig()
		notifications.InitializeNotifications()
	},
	Run: func(cmd *cobra.Command, args []string) {
		fileList := make([]string, 0)

		if len(args) == 0 {
			args = viper.GetStringSlice("paths")
		}

		if len(args) == 0 {
			log.Fatalf("You must supply at least a single path via CLI argument or PATHS env variable")
			return
		}

		for _, arg := range args {
			realBasePath, pattern := doublestar.SplitPattern(arg)
			files, err := doublestar.Glob(os.DirFS(realBasePath), pattern)

			if err != nil {
				log.Fatal(err)
			}

			log.Tracef("Found %s: %d", arg, len(files))

			for _, file := range files {
				absPath, err := filepath.Abs(filepath.Join(realBasePath, file))
				if err != nil {
					panic(err)
				}
				fileList = append(fileList, absPath)
			}
		}

		if len(fileList) == 0 {
			log.Error("Specified paths did not match any files")
		}

		skip := make(chan bool, 1)
		notifications.SetSkipChannel(skip)

		for _, fileName := range fileList {
			if terminated {
				return
			}

			if !shouldTranscode(fileName) {
				// File already processed
				continue
			}

			log.Infof("Transcoding: %s", fileName)
			metadata, err := transcoder.ReadFileMetadata(fileName)

			if err != nil {
				log.Infof("failed reading metadata: %s", err)
				continue
			}

			tempFileName := fileName + ".transcode-temp"

			_, err = os.Stat(tempFileName)

			if err != nil && !os.IsNotExist(err) {
				log.Errorf("Error reading file %s: %s", tempFileName, err)
				continue
			}

			if err == nil {
				log.Warningf("File is already being transcoded: %s", fileName)
				continue
			}

			killed, lastReport, skipped := transcoder.TranscodeFile(fileName, tempFileName, metadata, skip)

			if terminated {
				notifications.NotifyEnd(nil, nil, models.ResultError)
				continue
			}

			lastDot := strings.LastIndex(fileName, ".")
			extCorrectedOriginal := fileName[:lastDot] + outputFileExtension
			processedFileName := filepath.Dir(extCorrectedOriginal) + "/." + filepath.Base(extCorrectedOriginal) + ".processed"

			updateProcessedFile(tempFileName, processedFileName)

			if killed && !skipped {
				if !terminated {
					updateProcessedFile(fileName, processedFileName)
				}

				// Assume corrupted output file
				err := os.Remove(tempFileName)

				if err != nil && !os.IsNotExist(err) {
					log.Errorf("Error deleting file %s: %s", tempFileName, err)
					continue
				}

				if lastReport != nil {
					if int64(lastReport.TotalSize) > metadata.Format.SizeInt() {
						log.Infof("Kept original %s: %s < %s",
							fileName,
							utils.BytesHumanReadable(metadata.Format.SizeInt()),
							utils.BytesHumanReadable(int64(lastReport.TotalSize)),
						)

						notifications.NotifyEnd(nil, lastReport, models.ResultKeepOriginal)
					}

					skipConfidence := utils.SkipConfidenceMeta(metadata, lastReport.Frame, lastReport.TotalSize)
					if skipConfidence > viper.GetFloat64("skip-confidence") {
						log.Infof("Kept original %s: Skip confidence of %.2f",
							fileName,
							skipConfidence,
						)

						notifications.NotifyEnd(nil, lastReport, models.ResultKeepOriginal)
					}
				}

				continue
			}

			if skipped {
				updateProcessedFile(fileName, processedFileName)

				// Transcoded file was skipped
				err := os.Remove(tempFileName)

				if err != nil && !os.IsNotExist(err) {
					log.Errorf("Error deleting file %s: %s", tempFileName, err)
					continue
				}

				log.Infof("Skipped, kept original: %s", fileName)
				notifications.NotifyEnd(nil, lastReport, models.ResultSkipped)

				continue
			}

			resultMetadata, err := transcoder.ReadFileMetadata(tempFileName)
			if err != nil {
				log.Infof("failed reading metadata: %s", err)
				notifications.NotifyEnd(nil, lastReport, models.ResultError)
				continue
			}

			if viper.GetBool("keep-old") && resultMetadata.Format.SizeInt() > metadata.Format.SizeInt() {
				// Transcoded file is bigger than original
				err := os.Remove(tempFileName)

				updateProcessedFile(fileName, processedFileName)

				if err != nil && !os.IsNotExist(err) {
					log.Errorf("Error deleting file %s: %s", tempFileName, err)
					continue
				}

				log.Infof("Kept original %s: %s < %s",
					fileName,
					utils.BytesHumanReadable(metadata.Format.SizeInt()),
					utils.BytesHumanReadable(resultMetadata.Format.SizeInt()),
				)

				notifications.NotifyEnd(resultMetadata, nil, models.ResultKeepOriginal)
			} else if viper.GetBool("keep-old") && utils.SkipConfidenceMeta(metadata, resultMetadata.Frames(), int(resultMetadata.Format.SizeInt())) > viper.GetFloat64("skip-confidence") {
				// Transcoded file is skipped due to extrapolated data
				err := os.Remove(tempFileName)

				updateProcessedFile(fileName, processedFileName)

				if err != nil && !os.IsNotExist(err) {
					log.Errorf("Error deleting file %s: %s", tempFileName, err)
					continue
				}

				log.Infof("Kept original %s: Skip confidence of %.2f",
					fileName,
					utils.SkipConfidenceMeta(metadata, resultMetadata.Frames(), int(resultMetadata.Format.SizeInt())),
				)

				notifications.NotifyEnd(resultMetadata, nil, models.ResultKeepOriginal)
			} else {
				// Transcoded file is smaller than original
				err := os.Remove(fileName)

				if err != nil {
					log.Errorf("Error deleting file %s: %s", fileName, err)
					continue
				}

				err = os.Rename(tempFileName, extCorrectedOriginal)

				if err != nil {
					log.Errorf("Error renaming file %s to %s: %s", tempFileName, extCorrectedOriginal, err)
					continue
				}

				log.Infof("Replaced %s with transcoded: %s < %s",
					fileName,
					utils.BytesHumanReadable(resultMetadata.Format.SizeInt()),
					utils.BytesHumanReadable(metadata.Format.SizeInt()),
				)

				notifications.NotifyEnd(resultMetadata, nil, models.ResultReplaced)
			}

		}
	},
}

func Execute() {
	terminate := make(chan os.Signal)

	go func() {
		<-terminate
		terminated = true
	}()

	signal.Notify(terminate, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	rootCmd.PersistentFlags().StringVar(&LogLevel, "log", "info", "The log level to output")
	rootCmd.PersistentFlags().BoolVar(&ForceColors, "colors", false, "Force output with colors")

	rootCmd.PersistentFlags().StringP("flags", "f", "-map 0 -c:v libx265 -preset ultrafast -x265-params crf=16 -c:a aac -strict -2 -b:a 256k", "The base flags used for all transcodes")
	rootCmd.PersistentFlags().StringSliceP("extensions", "e", []string{".mp4", ".mkv", ".flv"}, "Transcoded file extensions")
	rootCmd.PersistentFlags().Int("interval", 5, "How often to output transcoding status")
	rootCmd.PersistentFlags().Bool("stderr", false, "Whether to output ffmpeg stderr stream")
	rootCmd.PersistentFlags().Bool("keep-old", true, "Keep old version of video if transcoded version is larger")
	rootCmd.PersistentFlags().Bool("early-exit", true, "Early exit if transcoded version is larger than original (requires keep-old)")
	rootCmd.PersistentFlags().Bool("nice", true, "Whether to lower the priority of ffmpeg process")
	rootCmd.PersistentFlags().Float64("skip-confidence", 15, "Skip confidence for early exit")

	rootCmd.PersistentFlags().String("tg-bot-key", "", "Telegram Bot API Key")
	rootCmd.PersistentFlags().String("tg-chat-id", "", "Telegram Bot Chat ID")
	rootCmd.PersistentFlags().Int("tg-admin-id", 0, "Telegram Admin User ID")

	_ = viper.BindPFlag("flags", rootCmd.PersistentFlags().Lookup("flags"))
	_ = viper.BindPFlag("extensions", rootCmd.PersistentFlags().Lookup("extensions"))
	_ = viper.BindPFlag("interval", rootCmd.PersistentFlags().Lookup("interval"))
	_ = viper.BindPFlag("stderr", rootCmd.PersistentFlags().Lookup("stderr"))
	_ = viper.BindPFlag("keep-old", rootCmd.PersistentFlags().Lookup("keep-old"))
	_ = viper.BindPFlag("early-exit", rootCmd.PersistentFlags().Lookup("early-exit"))
	_ = viper.BindPFlag("nice", rootCmd.PersistentFlags().Lookup("nice"))
	_ = viper.BindPFlag("skip-confidence", rootCmd.PersistentFlags().Lookup("skip-confidence"))

	_ = viper.BindPFlag("tg-bot-key", rootCmd.PersistentFlags().Lookup("tg-bot-key"))
	_ = viper.BindPFlag("tg-chat-id", rootCmd.PersistentFlags().Lookup("tg-chat-id"))
	_ = viper.BindPFlag("tg-admin-id", rootCmd.PersistentFlags().Lookup("tg-admin-id"))
}

func shouldTranscode(fileName string) bool {
	if terminated {
		return false
	}

	ext := filepath.Ext(fileName)

	valid := false
	for _, extension := range viper.GetStringSlice("extensions") {
		if ext == extension {
			valid = true
			break
		}
	}

	if !valid {
		return false
	}

	lastDot := strings.LastIndex(fileName, ".")
	extCorrectedOriginal := fileName[:lastDot] + outputFileExtension
	processedFileName := filepath.Dir(extCorrectedOriginal) + "/." + filepath.Base(extCorrectedOriginal) + ".processed"

	stat, err := os.Stat(processedFileName)

	if err != nil && !os.IsNotExist(err) {
		log.Errorf("Error reading file %s: %s", processedFileName, err)
		return false
	}

	if stat == nil {
		// File not transcoded ever
		return true
	}

	if stat.Size() == 0 {
		// File processed using old transcoder, update meta file and skip
		log.Warningf("Updating processed file with file size from old transcoder: %s", fileName)
		updateProcessedFile(fileName, processedFileName)
		return false
	}

	processedData, err := ioutil.ReadFile(processedFileName)

	if err != nil {
		log.Errorf("Error reading file %s: %s", processedFileName, err)
		return false
	}

	if len(processedData) == 0 {
		// File processed using old transcoder, update meta file and skip
		log.Warningf("Updating processed file with file size from old transcoder: %s", fileName)
		updateProcessedFile(fileName, processedFileName)
		return false
	}

	parsed, err := strconv.ParseInt(string(processedData), 10, 64)

	if err != nil {
		log.Errorf("Error parsing %s: %s", string(processedData), err)
		return false
	}

	originalStat, err := os.Stat(fileName)

	if err != nil {
		log.Errorf("Error reading file %s: %s", fileName, err)
		return false
	}

	if parsed == originalStat.Size() {
		return false
	}

	if !deleteProcessedFile(processedFileName) {
		return false
	}

	return true
}

func updateProcessedFile(fileName string, processedFileName string) {
	if !deleteProcessedFile(processedFileName) {
		return
	}

	originalStat, err := os.Stat(fileName)

	if os.IsNotExist(err) {
		return
	}

	if err != nil {
		log.Errorf("Error reading file %s: %s", fileName, err)
		return
	}

	err = ioutil.WriteFile(processedFileName, []byte(strconv.FormatInt(originalStat.Size(), 10)), 0644)

	if err != nil {
		log.Errorf("Error writing file %s: %s", processedFileName, err)
		return
	}
}

func deleteProcessedFile(processedFileName string) bool {
	err := os.Remove(processedFileName)

	if err != nil && !os.IsNotExist(err) {
		log.Errorf("Error deleting file %s: %s", processedFileName, err)
		return false
	}

	return true
}
