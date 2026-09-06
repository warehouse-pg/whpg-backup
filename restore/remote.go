package restore

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/greenplum-db/gpbackup/options"
	"github.com/pkg/errors"
	"github.com/warehouse-pg/common-go-libs/cluster"
	"github.com/warehouse-pg/common-go-libs/gplog"
	"github.com/warehouse-pg/common-go-libs/iohelper"
)

/*
 * Functions to run commands on entire cluster during restore
 */

/*
 * EnsureBackupDirectoriesExistOnAllHosts checks, or creates, the backup
 * directories the restore is about to use.
 *
 * Without a plugin those directories hold the backup itself, so a missing one
 * means the data is gone and the restore must stop.  With a plugin the backup
 * lives on the plugin's storage and the local directories are only staging
 * areas for files gprestore downloads into them, so they are created here
 * instead of being required.  That is what lets a backup be restored onto a
 * host that was rebuilt or replaced after the backup was taken, without having
 * to reconstruct the old directory tree by hand first.
 */
func EnsureBackupDirectoriesExistOnAllHosts() {
	usingPlugin := MustGetFlagString(options.PLUGIN_CONFIG) != ""

	coordinatorDir := globalFPInfo.GetDirForContent(-1)
	if usingPlugin {
		_, err := globalCluster.ExecuteLocalCommand(fmt.Sprintf("mkdir -p %s", coordinatorDir))
		gplog.FatalOnError(err, "Unable to create backup directory %s", coordinatorDir)
	} else {
		_, err := globalCluster.ExecuteLocalCommand(fmt.Sprintf("test -d %s", coordinatorDir))
		gplog.FatalOnError(err, "Backup directory %s missing or inaccessible", coordinatorDir)
	}

	// The segment directories are needed for every plugin restore, not just the
	// single-data-file ones: a resize restore stages the helper's pipes, oid
	// files and segment TOCs there as well.
	dirCommand, verboseMsg, errMsg := "test -d", "Verifying backup directories exist", "Backup directories missing or inaccessible"
	if usingPlugin {
		dirCommand, verboseMsg, errMsg = "mkdir -p", "Creating backup directories", "Unable to create backup directories"
	}
	origSize, destSize, isResizeRestore, _ := GetResizeClusterInfo()

	remoteOutput := globalCluster.GenerateAndExecuteCommand(verboseMsg, cluster.ON_SEGMENTS, func(contentID int) string {
		if isResizeRestore { // Map origin content to destination content to find where the original files have been placed
			if contentID >= origSize { // Don't check for directories for contents that aren't part of the backup set
				return ""
			}
			contentID = contentID % destSize
		}
		return fmt.Sprintf("%s %s", dirCommand, globalFPInfo.GetDirForContent(contentID))
	})
	globalCluster.CheckClusterError(remoteOutput, errMsg, func(contentID int) string {
		if usingPlugin {
			return fmt.Sprintf("Unable to create backup directory %s", globalFPInfo.GetDirForContent(contentID))
		}
		return fmt.Sprintf("Backup directory %s missing or inaccessible", globalFPInfo.GetDirForContent(contentID))
	})
}

func VerifyBackupFileCountOnSegments() {
	// In the current backup directory format, all content IDs are intermingled in one directory, so we need to get a list of which contents
	// correspond to the content ID we're going to check in order to provide a useful count to the user in the case of an error.
	origSize, destSize, isResizeRestore, _ := GetResizeClusterInfo()
	contentMap := make(map[int][]string, destSize) // []string instead of []int so we can join them later
	for i := 0; i < origSize; i++ {
		contentMap[i%destSize] = append(contentMap[i%destSize], fmt.Sprintf("%d", i))
	}

	remoteOutput := globalCluster.GenerateAndExecuteCommand("Verifying backup file count", cluster.ON_SEGMENTS, func(contentID int) string {
		// Coordinator backup files (and any gprestore report files) will be mixed in with segment backup files on a single-node cluster,
		// so we explicitly look for filenames in the segment filename format.  In a smaller-to-larger restore, the contents list for a segment
		// outside the destination array will be "[]", which the find command can handle safely in this context.
		contentsList := fmt.Sprintf("(%s)", strings.Join(contentMap[contentID], "|"))
		var cmdString string
		if runtime.GOOS == "linux" {
			cmdString = fmt.Sprintf(`find %s -type f -regextype posix-extended -regex ".*gpbackup_%s_%s.*" | wc -l`, globalFPInfo.GetDirForContent(contentID), contentsList, globalFPInfo.Timestamp)
		} else if runtime.GOOS == "darwin" {
			cmdString = fmt.Sprintf(`find -E %s -type f -regex ".*gpbackup_%s_%s.*" | wc -l`, globalFPInfo.GetDirForContent(contentID), contentsList, globalFPInfo.Timestamp)
		}
		return cmdString
	})
	globalCluster.CheckClusterError(remoteOutput, "Could not verify backup file count", func(contentID int) string {
		return "Could not verify backup file count"
	})

	// these are the file counts for non-resize restores.
	fileCount := 2 // 1 for the actual data file, 1 for the segment TOC file
	if !backupConfig.SingleDataFile {
		fileCount = len(globalTOC.DataEntries)
	}

	batchMap := make(map[int]int, len(remoteOutput.Commands))
	for i := 0; i < origSize; i++ {
		batchMap[i%destSize] += fileCount
	}

	numIncorrect := 0
	for contentID, cmd := range remoteOutput.Commands {
		numFound, _ := strconv.Atoi(strings.TrimSpace(cmd.Stdout))
		if isResizeRestore {
			fileCount = batchMap[contentID]
		}
		if numFound != fileCount {
			gplog.Verbose("Expected to find %d file(s) on segment %d on host %s, but found %d instead.", fileCount, contentID, globalCluster.GetHostForContent(contentID), numFound)
			numIncorrect++
		}
	}
	if numIncorrect > 0 {
		cluster.LogFatalClusterError("Found incorrect number of backup files", cluster.ON_SEGMENTS, numIncorrect)
	}
}

func VerifyMetadataFilePaths(withStats bool) {
	filetypes := []string{"config", "table of contents", "metadata"}
	missing := false
	for _, filetype := range filetypes {
		filepath := globalFPInfo.GetBackupFilePath(filetype)
		if !iohelper.FileExistsAndIsReadable(filepath) {
			missing = true
			gplog.Error("Cannot access %s file %s", filetype, filepath)
		}
	}
	if withStats {
		filepath := globalFPInfo.GetStatisticsFilePath()
		if !iohelper.FileExistsAndIsReadable(filepath) {
			missing = true
			gplog.Error("Cannot access statistics file %s", filepath)
			gplog.Error(`Note that the "-with-stats" flag must be passed to gpbackup to generate a statistics file.`)
		}
	}
	if missing {
		gplog.Fatal(errors.Errorf("One or more metadata files do not exist or are not readable."), "Cannot proceed with restore")
	}
}

func GetResizeClusterInfo() (int, int, bool, int) {
	isResizeCluster := MustGetFlagBool(options.RESIZE_CLUSTER)
	origSize := backupConfig.SegmentCount
	destSize := len(globalCluster.ContentIDs) - 1
	if !isResizeCluster && origSize == 0 { // Backup taken with version <1.26, no SegmentCount stored
		origSize = destSize
	}

	batches := 1
	if isResizeCluster && origSize > destSize {
		batches = origSize / destSize
		// If dest doesn't divide evenly into orig, there's one more incomplete batch
		if origSize%destSize != 0 {
			batches += 1
		}
	}

	return origSize, destSize, isResizeCluster, batches
}
