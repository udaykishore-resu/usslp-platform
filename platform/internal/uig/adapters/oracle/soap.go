package oracle

import (
	"encoding/xml"
	"net/http"
	"strconv"
)

// FaultCode is the SOAP 1.1 fault code, which is the field Oracle's RIB error
// hospital actually routes on.
//
// Client means "your message is wrong"; the hospital marks it for human
// attention and stops replaying it. Server means "we could not process a
// message that looks fine"; the hospital retries. Choosing between them
// correctly is the whole reason the UIG never answers a parse failure with a
// retryable status: a message that cannot be parsed replayed forever is a
// blocked queue and every price behind it is stuck.
type FaultCode string

const (
	// FaultClient marks a permanent, caller-side failure.
	FaultClient FaultCode = "soapenv:Client"
	// FaultServer marks a transient, platform-side failure worth retrying.
	FaultServer FaultCode = "soapenv:Server"
)

// ResponseNamespace is the namespace of the UIG's own response and fault detail
// elements.
const ResponseNamespace = "http://usslp.io/uig/v1"

type faultEnvelope struct {
	XMLName xml.Name  `xml:"soapenv:Envelope"`
	NS      string    `xml:"xmlns:soapenv,attr"`
	Body    faultBody `xml:"soapenv:Body"`
}

type faultBody struct {
	Fault soapFault `xml:"soapenv:Fault"`
}

type soapFault struct {
	Code   FaultCode    `xml:"faultcode"`
	String string       `xml:"faultstring"`
	Actor  string       `xml:"faultactor,omitempty"`
	Detail *faultDetail `xml:"detail,omitempty"`
}

type faultDetail struct {
	Error faultError `xml:"usslp:IngestError"`
}

type faultError struct {
	NS         string `xml:"xmlns:usslp,attr"`
	Reason     string `xml:"usslp:reason"`
	Detail     string `xml:"usslp:detail"`
	DeliveryID string `xml:"usslp:deliveryId,omitempty"`
	// Retryable states in the payload what the fault code states in the
	// envelope, because more than one RIB integration in the field keys off the
	// detail block rather than the code and both should agree.
	Retryable bool `xml:"usslp:retryable"`
}

// Fault renders a SOAP 1.1 fault.
//
// reason is the low-cardinality token that also appears in the UIG's metrics,
// so a retailer quoting the fault they received and an engineer looking at a
// dashboard are using the same word for the same thing.
func Fault(code FaultCode, reason, detail, deliveryID string) []byte {
	env := faultEnvelope{
		NS: SOAPNamespace,
		Body: faultBody{Fault: soapFault{
			Code:   code,
			String: detail,
			Actor:  "usslp-uig",
			Detail: &faultDetail{Error: faultError{
				NS:         ResponseNamespace,
				Reason:     reason,
				Detail:     detail,
				DeliveryID: deliveryID,
				Retryable:  code == FaultServer,
			}},
		}},
	}
	out, err := xml.MarshalIndent(env, "", "  ")
	if err != nil {
		// The structure is fixed and contains only strings and a bool, so this
		// cannot fail in practice; returning a minimal hand-built fault rather
		// than panicking keeps a marshalling bug from taking the endpoint down.
		return []byte(xml.Header +
			`<soapenv:Envelope xmlns:soapenv="` + SOAPNamespace + `"><soapenv:Body><soapenv:Fault>` +
			`<faultcode>` + string(code) + `</faultcode>` +
			`<faultstring>ingest failed</faultstring></soapenv:Fault></soapenv:Body></soapenv:Envelope>`)
	}
	return append([]byte(xml.Header), out...)
}

type responseEnvelope struct {
	XMLName xml.Name     `xml:"soapenv:Envelope"`
	NS      string       `xml:"xmlns:soapenv,attr"`
	Body    responseBody `xml:"soapenv:Body"`
}

type responseBody struct {
	Response acceptResponse `xml:"usslp:PublishItemPriceDescResponse"`
}

type acceptResponse struct {
	NS         string `xml:"xmlns:usslp,attr"`
	Status     string `xml:"usslp:status"`
	DeliveryID string `xml:"usslp:deliveryId"`
	Accepted   int    `xml:"usslp:changesAccepted"`
	Duplicate  bool   `xml:"usslp:duplicate"`
	// CorrelationID lets a retailer's support desk quote one identifier that
	// the platform can follow from this response to the shelf.
	CorrelationID string `xml:"usslp:correlationId,omitempty"`
}

// Response renders the success envelope RIB expects.
func Response(status, deliveryID, correlationID string, accepted int, duplicate bool) []byte {
	env := responseEnvelope{
		NS: SOAPNamespace,
		Body: responseBody{Response: acceptResponse{
			NS:            ResponseNamespace,
			Status:        status,
			DeliveryID:    deliveryID,
			Accepted:      accepted,
			Duplicate:     duplicate,
			CorrelationID: correlationID,
		}},
	}
	out, err := xml.MarshalIndent(env, "", "  ")
	if err != nil {
		return []byte(xml.Header +
			`<soapenv:Envelope xmlns:soapenv="` + SOAPNamespace + `"><soapenv:Body>` +
			`<usslp:PublishItemPriceDescResponse xmlns:usslp="` + ResponseNamespace + `">` +
			`<usslp:status>` + status + `</usslp:status>` +
			`<usslp:changesAccepted>` + strconv.Itoa(accepted) + `</usslp:changesAccepted>` +
			`</usslp:PublishItemPriceDescResponse></soapenv:Body></soapenv:Envelope>`)
	}
	return append([]byte(xml.Header), out...)
}

// FaultFor maps a pipeline HTTP status onto a SOAP fault code and the status
// the endpoint should return.
//
// The status translation is the point of this function. A 401, 404 or 422 stays
// as it is and carries a Client fault, because all three are permanent. A 503
// stays a 503 with a Server fault, because it is the one class a retry can fix.
// Nothing is promoted to 500: SOAP 1.1 says faults travel on 500, and RIB says
// 500 means retry, and between a specification and a queue that blocks, the
// queue wins.
func FaultFor(httpStatus int) (FaultCode, int) {
	if httpStatus >= 500 {
		return FaultServer, http.StatusServiceUnavailable
	}
	return FaultClient, httpStatus
}
