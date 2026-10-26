/* Play raw 16 kHz mono s16le on Vector MultiMedia2.
 * tinyplay ignores -n, so its ring stays 128ms and sentence gaps underrun.
 * period 1024 x 8 is 512ms and still consumes at realtime (measured on device).
 * The Q6 DSP crackles for the whole utterance if a write is shorter than one
 * period, so bytes are held until 2048 (1024 frames) are ready.
 * start_threshold is one period: the first word starts on that first write.
 */
#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>
#include <tinyalsa/pcm.h>

int main(int argc, char **argv) {
	if (argc < 2) {
		fprintf(stderr, "usage: xiaozhi-play <raw-s16le>\n");
		return 2;
	}
	FILE *in = fopen(argv[1], "rb");
	if (!in) {
		perror(argv[1]);
		return 1;
	}
	struct pcm_config cfg;
	memset(&cfg, 0, sizeof(cfg));
	cfg.channels = 1;
	cfg.rate = 16000;
	cfg.period_size = 1024;
	cfg.period_count = 8;
	cfg.format = PCM_FORMAT_S16_LE;
	cfg.start_threshold = 1024; /* one period, 64ms */
	struct pcm *pcm = pcm_open(0, 1, PCM_OUT, &cfg);
	if (!pcm || !pcm_is_ready(pcm)) {
		fprintf(stderr, "pcm_open: %s\n", pcm ? pcm_get_error(pcm) : "null");
		if (pcm)
			pcm_close(pcm);
		fclose(in);
		return 1;
	}
	int fd = fileno(in);
	unsigned char buf[2048];
	unsigned int fill = 0;
	for (;;) {
		ssize_t n = read(fd, buf + fill, sizeof(buf) - fill);
		if (n < 0) {
			if (errno == EINTR)
				continue;
			perror("read");
			pcm_close(pcm);
			fclose(in);
			return 1;
		}
		if (n == 0) {
			if (fill) {
				memset(buf + fill, 0, sizeof(buf) - fill);
				if (pcm_write(pcm, buf, sizeof(buf)) != 0) {
					fprintf(stderr, "pcm_write: %s\n", pcm_get_error(pcm));
					pcm_close(pcm);
					fclose(in);
					return 1;
				}
			}
			break;
		}
		fill += (unsigned int)n;
		if (fill < sizeof(buf))
			continue;
		if (pcm_write(pcm, buf, sizeof(buf)) != 0) {
			fprintf(stderr, "pcm_write: %s\n", pcm_get_error(pcm));
			pcm_close(pcm);
			fclose(in);
			return 1;
		}
		fill = 0;
	}
	pcm_close(pcm);
	fclose(in);
	return 0;
}
