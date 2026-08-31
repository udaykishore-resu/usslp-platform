#include "usslp_seq.h"

#include "../usslp_crc32c.h"

#include <string.h>

void usslp_seq_init(struct usslp_seq_state *s)
{
	s->displayed = INT64_MIN;
	s->accepted = 0;
	s->discarded = 0;
}

bool usslp_seq_never_displayed(const struct usslp_seq_state *s)
{
	return s->displayed == INT64_MIN;
}

enum usslp_seq_verdict usslp_seq_check(struct usslp_seq_state *s, int64_t candidate)
{
	if (usslp_seq_never_displayed(s)) {
		/* Anything is greater than nothing. A freshly provisioned label takes
		 * the first price it is offered, which is how commissioning works: the
		 * technician clips the label on and the controller pushes the planogram
		 * entry with whatever sequence that SKU is on. */
		return USSLP_SEQ_ACCEPT;
	}
	if (candidate <= s->displayed) {
		s->discarded++;
		return USSLP_SEQ_STALE;
	}
	return USSLP_SEQ_ACCEPT;
}

int usslp_seq_commit(struct usslp_seq_state *s, int64_t candidate)
{
	if (!usslp_seq_never_displayed(s) && candidate <= s->displayed) {
		return USSLP_ERR_STALE;
	}
	s->displayed = candidate;
	s->accepted++;
	return USSLP_OK;
}

static void put_be32(uint8_t *p, uint32_t v)
{
	p[0] = (uint8_t)(v >> 24);
	p[1] = (uint8_t)(v >> 16);
	p[2] = (uint8_t)(v >> 8);
	p[3] = (uint8_t)v;
}

static uint32_t get_be32(const uint8_t *p)
{
	return ((uint32_t)p[0] << 24) | ((uint32_t)p[1] << 16) | ((uint32_t)p[2] << 8) |
	       (uint32_t)p[3];
}

static void put_be64(uint8_t *p, uint64_t v)
{
	put_be32(p, (uint32_t)(v >> 32));
	put_be32(p + 4, (uint32_t)v);
}

static uint64_t get_be64(const uint8_t *p)
{
	return ((uint64_t)get_be32(p) << 32) | (uint64_t)get_be32(p + 4);
}

void usslp_seq_encode(const struct usslp_seq_state *s, uint8_t out[USSLP_SEQ_RECORD_LEN])
{
	put_be32(&out[0], USSLP_SEQ_MAGIC);
	put_be64(&out[4], (uint64_t)s->displayed);
	put_be32(&out[12], usslp_crc32c(out, 12));
}

int usslp_seq_decode(struct usslp_seq_state *s, const uint8_t in[USSLP_SEQ_RECORD_LEN])
{
	usslp_seq_init(s);
	if (get_be32(&in[0]) != USSLP_SEQ_MAGIC) {
		return USSLP_ERR_MALFORMED;
	}
	if (get_be32(&in[12]) != usslp_crc32c(in, 12)) {
		return USSLP_ERR_MALFORMED;
	}
	s->displayed = (int64_t)get_be64(&in[4]);
	return USSLP_OK;
}
