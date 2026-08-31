/*
 * The canonical price encoding. See usslp_canon.h for why this file is written
 * the way it is.
 */

#include "usslp_canon.h"

#include <string.h>

/* strnlen is POSIX rather than C11, and Zephyr's minimal libc does not always
 * provide it, so the bound check is written out. */
static size_t bounded_len(const char *s, size_t max)
{
	size_t n = 0;

	while (n <= max && s[n] != '\0') {
		n++;
	}
	return n;
}

size_t usslp_canon_format_i64(int64_t v, char *out, size_t cap)
{
	char tmp[20];
	size_t n = 0;
	size_t len;
	bool neg = v < 0;
	/* Negate in unsigned space: -INT64_MIN is undefined in signed arithmetic
	 * and INT64_MIN is a legal sequence value on the wire. */
	uint64_t mag = neg ? (~(uint64_t)v + 1u) : (uint64_t)v;

	if (cap == 0) {
		return 0;
	}
	do {
		tmp[n++] = (char)('0' + (mag % 10u));
		mag /= 10u;
	} while (mag != 0u);

	len = n + (neg ? 1u : 0u);
	if (cap < len + 1u) {
		return 0;
	}
	{
		size_t i = 0;
		if (neg) {
			out[i++] = '-';
		}
		while (n > 0) {
			out[i++] = tmp[--n];
		}
		out[i] = '\0';
		return i;
	}
}

/*
 * civil_from_days is Howard Hinnant's days-to-civil algorithm, valid for the
 * whole int64 range and, crucially, correct for days before the epoch. Go's
 * time package gets the same answer; a firmware that only handles positive
 * epochs quietly mis-encodes any label whose effective-at was backdated before
 * 1970, which sounds impossible until a data migration writes a zero-valued
 * timestamp and every label in the estate refuses the batch.
 */
static void civil_from_days(int64_t z, int64_t *y, unsigned *m, unsigned *d)
{
	int64_t era, doe, yoe, doy, mp;
	unsigned mm;

	z += 719468; /* shift the epoch to 0000-03-01 */
	era = (z >= 0 ? z : z - 146096) / 146097;
	doe = z - era * 146097;                                        /* [0, 146096] */
	yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;    /* [0, 399] */
	doy = doe - (365 * yoe + yoe / 4 - yoe / 100);                  /* [0, 365] */
	mp = (5 * doy + 2) / 153;                                       /* [0, 11] */
	*d = (unsigned)(doy - (153 * mp + 2) / 5 + 1);                  /* [1, 31] */
	mm = (unsigned)(mp < 10 ? mp + 3 : mp - 9);                     /* [1, 12] */
	*m = mm;
	*y = yoe + era * 400 + (mm <= 2 ? 1 : 0);
}

static void put2(char *out, unsigned v)
{
	out[0] = (char)('0' + (v / 10u) % 10u);
	out[1] = (char)('0' + v % 10u);
}

size_t usslp_canon_format_rfc3339_utc(int64_t unix_seconds, char *out, size_t cap)
{
	int64_t days, secs, year;
	unsigned month, day, hh, mm, ss;

	if (cap < 21u) {
		return 0;
	}
	/* Floor division: C truncates toward zero, which for a pre-epoch instant
	 * would put the time-of-day on the wrong side of midnight. */
	days = unix_seconds / 86400;
	secs = unix_seconds % 86400;
	if (secs < 0) {
		secs += 86400;
		days -= 1;
	}
	civil_from_days(days, &year, &month, &day);
	if (year < 0 || year > 9999) {
		/* Go would render a five-digit or negative year, which this fixed-width
		 * encoder cannot reproduce. Refusing is right: silently emitting a
		 * different string is exactly the divergence this module exists to
		 * prevent. */
		return 0;
	}
	hh = (unsigned)(secs / 3600);
	mm = (unsigned)((secs / 60) % 60);
	ss = (unsigned)(secs % 60);

	out[0] = (char)('0' + (unsigned)(year / 1000) % 10u);
	out[1] = (char)('0' + (unsigned)(year / 100) % 10u);
	out[2] = (char)('0' + (unsigned)(year / 10) % 10u);
	out[3] = (char)('0' + (unsigned)year % 10u);
	out[4] = '-';
	put2(&out[5], month);
	out[7] = '-';
	put2(&out[8], day);
	out[10] = 'T';
	put2(&out[11], hh);
	out[13] = ':';
	put2(&out[14], mm);
	out[16] = ':';
	put2(&out[17], ss);
	out[19] = 'Z';
	out[20] = '\0';
	return 20;
}

/*
 * The field walk. Both the string builder and the streaming digest use it, so
 * there is exactly one description of the encoding in this firmware and the two
 * entry points cannot drift apart.
 *
 * emit is called once per field and once per separator, in order.
 */
struct canon_sink {
	int (*emit)(void *ctx, const char *data, size_t len);
	void *ctx;
};

static int canon_walk(const struct usslp_price_input *in, const struct canon_sink *sink)
{
	char num[21];
	char ts[21];
	size_t n;
	int rc;

	const char *ids[5];
	size_t idlen[5];

	if (in == NULL || in->tenant == NULL || in->store == NULL || in->label == NULL ||
	    in->sku == NULL || in->currency == NULL || in->promotion == NULL) {
		return USSLP_ERR_INVAL;
	}

	ids[0] = in->tenant;
	ids[1] = in->store;
	ids[2] = in->label;
	ids[3] = in->sku;
	ids[4] = in->promotion;
	for (unsigned i = 0; i < 5; i++) {
		idlen[i] = bounded_len(ids[i], USSLP_CANON_MAX_ID);
		if (idlen[i] > USSLP_CANON_MAX_ID) {
			return USSLP_ERR_INVAL;
		}
	}
	if (bounded_len(in->currency, 3) != 3u) {
		/* canon.Money.Valid requires exactly three alphabetic characters. A
		 * shorter or longer code cannot have been signed by the platform, so
		 * there is nothing to be gained by hashing it. */
		return USSLP_ERR_INVAL;
	}
	for (unsigned i = 0; i < 3; i++) {
		if (in->currency[i] < 'A' || in->currency[i] > 'Z') {
			return USSLP_ERR_INVAL;
		}
	}
	if (usslp_canon_format_rfc3339_utc(in->effective_at, ts, sizeof(ts)) != 20u) {
		return USSLP_ERR_INVAL;
	}

#define EMIT(p, l)                                                                                 \
	do {                                                                                       \
		rc = sink->emit(sink->ctx, (p), (l));                                              \
		if (rc != USSLP_OK) {                                                              \
			return rc;                                                                 \
		}                                                                                  \
	} while (0)
#define SEP() EMIT("|", 1)

	EMIT(USSLP_CANON_SCHEME, sizeof(USSLP_CANON_SCHEME) - 1u);
	SEP();
	EMIT(ids[0], idlen[0]); /* tenant */
	SEP();
	EMIT(ids[1], idlen[1]); /* store */
	SEP();
	EMIT(ids[2], idlen[2]); /* label */
	SEP();
	EMIT(ids[3], idlen[3]); /* sku */
	SEP();
	n = usslp_canon_format_i64(in->amount_minor, num, sizeof(num));
	if (n == 0u) {
		return USSLP_ERR_INVAL;
	}
	EMIT(num, n);
	SEP();
	EMIT(in->currency, 3);
	SEP();
	EMIT(ts, 20);
	SEP();
	n = usslp_canon_format_i64(in->sequence, num, sizeof(num));
	if (n == 0u) {
		return USSLP_ERR_INVAL;
	}
	EMIT(num, n);
	SEP();
	EMIT(ids[4], idlen[4]); /* promotion, frequently empty */

#undef SEP
#undef EMIT
	return USSLP_OK;
}

struct buf_sink {
	char *out;
	size_t cap;
	size_t len;
};

static int buf_emit(void *ctx, const char *data, size_t len)
{
	struct buf_sink *s = (struct buf_sink *)ctx;

	if (s->len + len + 1u > s->cap) {
		return USSLP_ERR_NOSPACE;
	}
	memcpy(s->out + s->len, data, len);
	s->len += len;
	return USSLP_OK;
}

int usslp_canon_price_string(const struct usslp_price_input *in, char *out, size_t cap,
			     size_t *out_len)
{
	struct buf_sink s = { .out = out, .cap = cap, .len = 0 };
	struct canon_sink sink = { .emit = buf_emit, .ctx = &s };
	int rc;

	if (out == NULL || cap == 0u) {
		return USSLP_ERR_INVAL;
	}
	rc = canon_walk(in, &sink);
	if (rc != USSLP_OK) {
		out[0] = '\0';
		if (out_len != NULL) {
			*out_len = 0;
		}
		return rc;
	}
	out[s.len] = '\0';
	if (out_len != NULL) {
		*out_len = s.len;
	}
	return USSLP_OK;
}

static int hash_emit(void *ctx, const char *data, size_t len)
{
	usslp_sha256_update((struct usslp_sha256 *)ctx, data, len);
	return USSLP_OK;
}

int usslp_canon_price_digest(const struct usslp_price_input *in,
			     uint8_t digest[USSLP_SHA256_DIGEST_LEN])
{
	struct usslp_sha256 ctx;
	struct canon_sink sink = { .emit = hash_emit, .ctx = &ctx };
	int rc;

	usslp_sha256_init(&ctx);
	rc = canon_walk(in, &sink);
	if (rc != USSLP_OK) {
		/* Finalise anyway so the context is wiped, and throw the result away.
		 * Returning a partial digest to a caller that ignored the error code
		 * would be a way to verify against half a price. */
		uint8_t scratch[USSLP_SHA256_DIGEST_LEN];

		usslp_sha256_final(&ctx, scratch);
		memset(scratch, 0, sizeof(scratch));
		memset(digest, 0, USSLP_SHA256_DIGEST_LEN);
		return rc;
	}
	usslp_sha256_final(&ctx, digest);
	return USSLP_OK;
}
