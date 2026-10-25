package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CreditReservation is one reservation as the API views it
// (apps/api/src/routes/credit-reservations.ts, view()).
//
// AmountCents is everything ever set aside on it; ConsumedCents is what its
// database has drawn from it since, which metering moves every hour.
// InstanceID is null once the database has been deleted.
type CreditReservation struct {
	ID                string  `json:"id"`
	InstanceID        *string `json:"instance_id"`
	InstanceName      *string `json:"instance_name"`
	InstanceStatus    *string `json:"instance_status"`
	AmountCents       int64   `json:"amount_cents"`
	ConsumedCents     int64   `json:"consumed_cents"`
	RemainingCents    int64   `json:"remaining_cents"`
	Status            string  `json:"status"`
	DeactivatedAt     *string `json:"deactivated_at"`
	DeactivatedReason *string `json:"deactivated_reason"`
	CreatedAt         string  `json:"created_at"`
}

// CreditReservations is GET /credits/reservations: the balance, how much of it
// is set aside, and every reservation.
type CreditReservations struct {
	BalanceCents    int64               `json:"balance_cents"`
	ReservedCents   int64               `json:"reserved_cents"`
	UnreservedCents int64               `json:"unreserved_cents"`
	Reservations    []CreditReservation `json:"reservations"`
}

// ReservationAmount is the API's amountSchema: months of the database's price,
// or cents. Exactly one.
type ReservationAmount struct {
	Months      *int64 `json:"months,omitempty"`
	AmountCents *int64 `json:"amount_cents,omitempty"`
}

// ReservationPreviewRequest is POST /credits/reservations/preview.
type ReservationPreviewRequest struct {
	InstanceID string `json:"instance_id"`
	ReservationAmount
	AddToReservationID   string `json:"add_to_reservation_id,omitempty"`
	ReleaseReservationID string `json:"release_reservation_id,omitempty"`
}

// ReservationPreview is what a create, add or replacement would do, from the
// same checks the API applies to them.
type ReservationPreview struct {
	Allowed bool `json:"allowed"`
	Refusal *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"refusal"`
	AmountCents                int64    `json:"amount_cents"`
	BalanceCents               int64    `json:"balance_cents"`
	UnreservedBeforeCents      int64    `json:"unreserved_before_cents"`
	UnreservedAfterCents       int64    `json:"unreserved_after_cents"`
	UnreservedDailyCentsBefore int64    `json:"unreserved_daily_cents_before"`
	UnreservedDailyCentsAfter  int64    `json:"unreserved_daily_cents_after"`
	UnreservedRunwayDaysBefore *float64 `json:"unreserved_runway_days_before"`
	UnreservedRunwayDaysAfter  *float64 `json:"unreserved_runway_days_after"`
	AtRisk                     []struct {
		InstanceID string  `json:"instance_id"`
		Name       *string `json:"name"`
		DailyCents int64   `json:"daily_cents"`
	} `json:"at_risk"`
}

func reservationPath(id string) string {
	return "/credits/reservations/" + url.PathEscape(id)
}

func (c *Client) ListCreditReservations(ctx context.Context) (*CreditReservations, error) {
	var out CreditReservations
	if err := c.do(ctx, http.MethodGet, "/credits/reservations", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCreditReservation finds one reservation in the list. The API has no read
// of a single reservation; one that is not listed answers as a 404, so a
// reservation deleted elsewhere drops from state like any other resource.
func (c *Client) GetCreditReservation(ctx context.Context, id string) (*CreditReservation, error) {
	all, err := c.ListCreditReservations(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all.Reservations {
		if strings.EqualFold(all.Reservations[i].ID, id) {
			return &all.Reservations[i], nil
		}
	}
	return nil, &APIError{Status: http.StatusNotFound, Code: "not_found", Message: "No such reservation."}
}

func (c *Client) reservationCall(ctx context.Context, method, path string, body any) (*CreditReservation, error) {
	var out struct {
		Reservation *CreditReservation `json:"reservation"`
	}
	if err := c.do(ctx, method, path, body, &out); err != nil {
		return nil, err
	}
	if out.Reservation == nil {
		return nil, fmt.Errorf("the API answered %s %s without the reservation", method, path)
	}
	return out.Reservation, nil
}

// CreateCreditReservation sets credit aside for one database. Nothing is
// spent: the amount leaves the unreserved balance.
func (c *Client) CreateCreditReservation(ctx context.Context, instanceID string, amt ReservationAmount) (*CreditReservation, error) {
	body := struct {
		InstanceID string `json:"instance_id"`
		ReservationAmount
	}{instanceID, amt}
	return c.reservationCall(ctx, http.MethodPost, "/credits/reservations", body)
}

// AddToCreditReservation sets more aside on an active reservation.
func (c *Client) AddToCreditReservation(ctx context.Context, id string, amt ReservationAmount) (*CreditReservation, error) {
	return c.reservationCall(ctx, http.MethodPost, reservationPath(id)+"/add", amt)
}

// ActivateCreditReservation protects the database again with what is left.
func (c *Client) ActivateCreditReservation(ctx context.Context, id string) (*CreditReservation, error) {
	return c.reservationCall(ctx, http.MethodPost, reservationPath(id)+"/activate", nil)
}

// DeactivateCreditReservation stops protecting the database; what is left
// returns to the unreserved balance.
func (c *Client) DeactivateCreditReservation(ctx context.Context, id string) (*CreditReservation, error) {
	return c.reservationCall(ctx, http.MethodPost, reservationPath(id)+"/deactivate", nil)
}

// DeleteCreditReservation removes an inactive reservation (409 still_active
// otherwise).
func (c *Client) DeleteCreditReservation(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, reservationPath(id), nil, nil)
}

// PreviewCreditReservation runs the create/add checks without changing anything.
func (c *Client) PreviewCreditReservation(ctx context.Context, req ReservationPreviewRequest) (*ReservationPreview, error) {
	var out ReservationPreview
	if err := c.do(ctx, http.MethodPost, "/credits/reservations/preview", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
