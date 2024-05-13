//go:build ignore

#include <linux/bpf.h>
#include <bpf_helpers.h>

SEC("xdp")
int xdp_prog_func(struct xdp_md *ctx) {
	void *data_end = (void *)(long)ctx->data_end;
	void *data     = (void *)(long)ctx->data;

	return XDP_PASS;
}