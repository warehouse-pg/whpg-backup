//go:build gpbackup
// +build gpbackup

package main

import (
	"os"

	. "github.com/greenplum-db/gpbackup/backup"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/spf13/cobra"
)

func main() {
	var rootCmd = &cobra.Command{
		Use:     "gpbackup",
		Short:   "gpbackup is the parallel backup utility for Greenplum",
		Args:    cobra.NoArgs,
		Version: GetVersion(),
		Run: func(cmd *cobra.Command, args []string) {
			defer DoTeardown()
			UseCmdFlags(cmd.Flags())
			DoFlagValidation(cmd)
			DoSetup()
			DoBackup()
		}}

	var deleteBackupCmd = &cobra.Command{
		Use:   "delete-backup <timestamp>",
		Short: "Delete a backup set",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			defer DoDeleteBackupTeardown()
			UseCmdFlags(cmd.Flags())
			DoDeleteBackup(args[0])
		}}
	rootCmd.AddCommand(deleteBackupCmd)

	var deleteBackupsBeforeCmd = &cobra.Command{
		Use:   "delete-backups-before <timestamp>",
		Short: "Delete full backups older than a given timestamp",
		Long: "Delete every full backup older than <timestamp> that has no live dependent backup.\n" +
			"Incremental backups are never deleted by this command, and a full backup with a live\n" +
			"incremental dependent is skipped (with a warning) until that dependent is gone. Use\n" +
			"delete-backup --cascade to remove an incremental chain explicitly.",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			defer DoDeleteBackupTeardown()
			UseCmdFlags(cmd.Flags())
			DoDeleteBackupsBefore(args[0])
		}}
	rootCmd.AddCommand(deleteBackupsBeforeCmd)

	var listBackupsCmd = &cobra.Command{
		Use:   "list-backups",
		Short: "List backups recorded in the gpbackup history database",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			defer DoListBackupsTeardown()
			UseCmdFlags(cmd.Flags())
			DoListBackups()
		}}
	rootCmd.AddCommand(listBackupsCmd)

	rootCmd.SetArgs(options.HandleSingleDashes(os.Args[1:]))
	DoInit(rootCmd)
	DoDeleteBackupInit(deleteBackupCmd)
	DoDeleteBackupsBeforeInit(deleteBackupsBeforeCmd)
	DoListBackupsInit(listBackupsCmd)
	if err := rootCmd.Execute(); err != nil {
		os.Exit(2)
	}
}
