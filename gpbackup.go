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
		Short: "Delete all backup sets older than a given timestamp",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			defer DoDeleteBackupTeardown()
			UseCmdFlags(cmd.Flags())
			DoDeleteBackupsBefore(args[0])
		}}
	rootCmd.AddCommand(deleteBackupsBeforeCmd)

	rootCmd.SetArgs(options.HandleSingleDashes(os.Args[1:]))
	DoInit(rootCmd)
	DoDeleteBackupInit(deleteBackupCmd)
	DoDeleteBackupsBeforeInit(deleteBackupsBeforeCmd)
	if err := rootCmd.Execute(); err != nil {
		os.Exit(2)
	}
}
