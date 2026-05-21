package helper

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// gpbackup writes OIDs in table-discovery order (which can be non-
// ascending, for instance user tables ahead of extension config dump
// tables that happen to have lower OIDs).
// If the helper sorted the list, it could look for the pipe for the
// smallest OID first, while gpbackup only created the pipe for the
// first-discovered OID. This in turn would lead the helper to fail
// with "no such file or directory", and gpbackup would hang forever.
func TestGetOidListFromFile_PreservesFileOrder(t *testing.T) {
	dir := t.TempDir()
	oidFile := filepath.Join(dir, "oid_list")
	// OIDs in non-ascending order: any future sort on the read path
	// would reorder these and trip the assertion below.
	if err := os.WriteFile(oidFile, []byte("3\n1\n2\n"), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := getOidListFromFile(oidFile)
	if err != nil {
		t.Fatalf("getOidListFromFile(%s): %v", oidFile, err)
	}

	want := []int{3, 1, 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("getOidListFromFile reordered OIDs: got %v, want %v (the file's order)", got, want)
	}
}
