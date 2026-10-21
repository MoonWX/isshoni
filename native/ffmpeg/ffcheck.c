/*
 * ffcheck: loads the FFmpeg DLLs it is linked against and writes what they report as ffmpeg-config.json, then fails
 * unless they are the LGPL build isshoni ships (docs/m2/06-windows-release.md, section 4.4). ASCII only: MSVC
 * reads this file in the system code page.
 *
 *   ffcheck <ffmpeg-config.json>
 *
 * Built by native/ffmpeg/build.sh with "cl -MT" against the build's include/ and lib/ import libraries, and run next
 * to avcodec-<C>.dll and avutil-<U>.dll. Exit status: 0 when every check passes, 1 when one fails (each problem is on
 * stderr and in the file's "problems"), 2 on a usage or I/O error.
 */
#define _CRT_SECURE_NO_WARNINGS
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <libavcodec/avcodec.h>
#include <libavutil/avutil.h>

#define WANT_LICENSE "LGPL version 2.1 or later"
#define MAX_NAMES 64
#define MAX_PROBLEMS 16

/* Sorted, as ffcheck sorts what av_codec_iterate returns. */
static const char *const want_encoders[] = {"h264_amf", "h264_mf", "h264_nvenc", "h264_qsv"};
static const char *const forbidden_options[] = {"--enable-gpl", "--enable-nonfree", "--enable-version3"};

static char problems[MAX_PROBLEMS][256];
static int nproblems;

static void problem(const char *what, const char *detail) {
    if (nproblems < MAX_PROBLEMS)
        snprintf(problems[nproblems++], sizeof problems[0], "%s%s", what, detail);
}

static int cmp_names(const void *a, const void *b) {
    return strcmp(*(const char *const *)a, *(const char *const *)b);
}

static void json_str(FILE *f, const char *s) {
    fputc('"', f);
    for (; *s; s++) {
        unsigned char c = (unsigned char)*s;
        if (c == '"' || c == '\\')
            fprintf(f, "\\%c", c);
        else if (c < 0x20)
            fprintf(f, "\\u%04x", c);
        else
            fputc(c, f);
    }
    fputc('"', f);
}

static void json_list(FILE *f, const char *key, const char *const *names, int n, int last) {
    fprintf(f, "  \"%s\": [", key);
    for (int i = 0; i < n; i++) {
        fputs(i ? ", " : "", f);
        json_str(f, names[i]);
    }
    fprintf(f, "]%s\n", last ? "" : ",");
}

static void json_version(FILE *f, const char *lib, unsigned v) {
    fprintf(f, "  \"%sVersion\": %u,\n  \"%sMajor\": %u,\n  \"%sMinor\": %u,\n  \"%sMicro\": %u,\n", lib, v, lib,
            AV_VERSION_MAJOR(v), lib, AV_VERSION_MINOR(v), lib, AV_VERSION_MICRO(v));
}

int main(int argc, char **argv) {
    const char *encoders[MAX_NAMES], *decoders[MAX_NAMES];
    int nenc = 0, ndec = 0;
    const AVCodec *codec;
    void *it = NULL;
    char num[32];

    if (argc != 2) {
        fprintf(stderr, "usage: ffcheck <ffmpeg-config.json>\n");
        return 2;
    }

    unsigned avcodec = avcodec_version(), avutil = avutil_version();
    const char *configuration = avcodec_configuration(), *license = avcodec_license();
    const char *avutil_lic = avutil_license();

    while ((codec = av_codec_iterate(&it)) != NULL) {
        if (av_codec_is_encoder(codec) && nenc < MAX_NAMES)
            encoders[nenc++] = codec->name;
        if (av_codec_is_decoder(codec) && ndec < MAX_NAMES)
            decoders[ndec++] = codec->name;
    }
    qsort(encoders, (size_t)nenc, sizeof encoders[0], cmp_names);
    qsort(decoders, (size_t)ndec, sizeof decoders[0], cmp_names);

    if (strcmp(license, WANT_LICENSE) != 0)
        problem("avcodec_license() is not \"" WANT_LICENSE "\": ", license);
    if (strcmp(avutil_lic, WANT_LICENSE) != 0)
        problem("avutil_license() is not \"" WANT_LICENSE "\": ", avutil_lic);
    for (size_t i = 0; i < sizeof forbidden_options / sizeof forbidden_options[0]; i++)
        if (strstr(configuration, forbidden_options[i]) || strstr(avutil_configuration(), forbidden_options[i]))
            problem("the configuration contains ", forbidden_options[i]);
    int same = nenc == (int)(sizeof want_encoders / sizeof want_encoders[0]);
    for (int i = 0; same && i < nenc; i++)
        same = strcmp(encoders[i], want_encoders[i]) == 0;
    if (!same)
        problem("the encoders are not exactly h264_amf, h264_mf, h264_nvenc, h264_qsv", "");
    if (ndec > 0)
        problem("the build has decoders, the first is ", decoders[0]);
    if (AV_VERSION_MAJOR(avcodec) != LIBAVCODEC_VERSION_MAJOR) {
        snprintf(num, sizeof num, "%u", AV_VERSION_MAJOR(avcodec));
        problem("avcodec_version() has another major than the headers: ", num);
    }
    if (AV_VERSION_MAJOR(avutil) != LIBAVUTIL_VERSION_MAJOR) {
        snprintf(num, sizeof num, "%u", AV_VERSION_MAJOR(avutil));
        problem("avutil_version() has another major than the headers: ", num);
    }

    FILE *f = fopen(argv[1], "wb");
    if (f == NULL) {
        perror(argv[1]);
        return 2;
    }
    fputs("{\n", f);
    json_version(f, "avcodec", avcodec);
    json_version(f, "avutil", avutil);
    fputs("  \"license\": ", f);
    json_str(f, license);
    fputs(",\n  \"avutilLicense\": ", f);
    json_str(f, avutil_lic);
    fputs(",\n  \"configuration\": ", f);
    json_str(f, configuration);
    fputs(",\n", f);
    json_list(f, "encoders", encoders, nenc, 0);
    json_list(f, "decoders", decoders, ndec, 0);
    fprintf(f, "  \"ok\": %s,\n", nproblems ? "false" : "true");
    const char *list[MAX_PROBLEMS];
    for (int i = 0; i < nproblems; i++)
        list[i] = problems[i];
    json_list(f, "problems", list, nproblems, 1);
    fputs("}\n", f);
    if (fclose(f) != 0) {
        perror(argv[1]);
        return 2;
    }

    for (int i = 0; i < nproblems; i++)
        fprintf(stderr, "ffcheck: %s\n", problems[i]);
    printf("ffcheck: libavcodec %u.%u.%u, libavutil %u.%u.%u, %s, %d encoders, %s\n", AV_VERSION_MAJOR(avcodec),
           AV_VERSION_MINOR(avcodec), AV_VERSION_MICRO(avcodec), AV_VERSION_MAJOR(avutil), AV_VERSION_MINOR(avutil),
           AV_VERSION_MICRO(avutil), license, nenc, nproblems ? "FAILED" : "ok");
    return nproblems ? 1 : 0;
}
