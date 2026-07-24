package llamacpp

import "testing"

func TestParseRuntime(t *testing.T) {
	tests := []struct {
		name        string
		logs        string
		wantDevice  string
		wantOffload int
		wantTotal   int
		wantParsed  bool
	}{
		{
			name:       "metal full offload",
			logs:       "ggml_metal_init: using Metal backend\nload_tensors: offloaded 33/33 layers to GPU",
			wantDevice: "metal", wantOffload: 33, wantTotal: 33, wantParsed: true,
		},
		{
			name:       "cpu fallback",
			logs:       "llm_load_tensors: offloaded 0/33 layers to GPU",
			wantDevice: "cpu", wantOffload: 0, wantTotal: 33, wantParsed: true,
		},
		{
			name:       "cuda partial",
			logs:       "ggml_cuda_init: found 1 CUDA devices\noffloaded 20/40 layers to GPU",
			wantDevice: "cuda", wantOffload: 20, wantTotal: 40, wantParsed: true,
		},
		{
			name:       "no markers yet",
			logs:       "loading model...",
			wantDevice: "cpu", wantOffload: 0, wantTotal: 0, wantParsed: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dev, off, total, parsed := parseRuntime(tc.logs)
			if dev != tc.wantDevice || off != tc.wantOffload || total != tc.wantTotal || parsed != tc.wantParsed {
				t.Fatalf("got (%q,%d,%d,%v), want (%q,%d,%d,%v)",
					dev, off, total, parsed, tc.wantDevice, tc.wantOffload, tc.wantTotal, tc.wantParsed)
			}
		})
	}
}

func TestGPULayersArg(t *testing.T) {
	cases := map[int]int{0: 999, -1: 0, 5: 5, 33: 33}
	for in, want := range cases {
		if got := gpuLayersArg(in); got != want {
			t.Errorf("gpuLayersArg(%d)=%d, want %d", in, got, want)
		}
	}
}
