package postgres

import "testing"

func TestInitdbBlockedBySharedMemory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name: "exact sandbox restriction",
			output: "FATAL: could not create shared memory segment: Operation not permitted\n" +
				"DETAIL: Failed system call was shmget(key=123, size=56, 03600).",
			want: true,
		},
		{
			name: "exact system shared memory exhaustion",
			output: "FATAL: could not create shared memory segment: No space left on device\n" +
				"DETAIL: Failed system call was shmget(key=123, size=56, 03600).\n" +
				"HINT: This occurs if all available shared memory IDs have been taken.",
			want: true,
		},
		{
			name: "disk space error is not shared memory exhaustion",
			output: "FATAL: could not create shared memory segment: No space left on device\n" +
				"DETAIL: Failed system call was shmget(key=123, size=56, 03600).",
		},
		{
			name:   "socket path too long",
			output: `LOG: Unix-domain socket path "/a/long/path" is too long (maximum 103 bytes)`,
		},
		{
			name:   "generic permission failure",
			output: `could not create directory "/bad": Permission denied`,
		},
		{
			name:   "unrelated operation not permitted",
			output: `could not open configuration file: Operation not permitted`,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := initdbBlockedBySharedMemory(test.output); got != test.want {
				t.Fatalf("initdbBlockedBySharedMemory() = %v, want %v", got, test.want)
			}
		})
	}
}
