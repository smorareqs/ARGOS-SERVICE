package properties

import (
	"context"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Propietarios (alcance del rol owner) y colaboradores asignados (alcance de
// limpieza y mantenimiento). Se reemplazan completos: la lista que llega es la
// que queda.

// SetAssignees deja exactamente esas personas asignadas a la propiedad.
func (s *Service) SetAssignees(ctx context.Context, actor authz.Actor, propertyID uuid.UUID, users []uuid.UUID) (PropertyRead, error) {
	return s.setPeople(ctx, actor, relAssigned, propertyID, users)
}

// SetOwners deja exactamente esos propietarios en la propiedad.
func (s *Service) SetOwners(ctx context.Context, actor authz.Actor, propertyID uuid.UUID, users []uuid.UUID) (PropertyRead, error) {
	return s.setPeople(ctx, actor, relOwner, propertyID, users)
}

// SetUserAssignments deja exactamente esas propiedades asignadas a la persona.
func (s *Service) SetUserAssignments(ctx context.Context, actor authz.Actor, userID uuid.UUID, props []uuid.UUID) (UserProperties, error) {
	return s.setProperties(ctx, actor, relAssigned, userID, props)
}

// SetUserOwnerships deja exactamente esas propiedades como propias de la
// persona.
func (s *Service) SetUserOwnerships(ctx context.Context, actor authz.Actor, userID uuid.UUID, props []uuid.UUID) (UserProperties, error) {
	return s.setProperties(ctx, actor, relOwner, userID, props)
}

func (s *Service) setPeople(ctx context.Context, actor authz.Actor, rel relation, propertyID uuid.UUID, users []uuid.UUID) (PropertyRead, error) {
	users = uniqueIDs(users)
	return database.InTxValue(ctx, s.db, func(q database.Querier) (PropertyRead, error) {
		repo := NewRepository(q)
		if _, err := s.visible(ctx, q, actor, propertyID); err != nil {
			return PropertyRead{}, err
		}
		n, err := repo.CountMembers(ctx, actor.TenantID, users)
		if err != nil {
			return PropertyRead{}, err
		}
		if n != len(users) {
			return PropertyRead{}, ErrNotMembers.WithField("body.user_ids")
		}
		before, err := repo.PeopleOf(ctx, rel, propertyID)
		if err != nil {
			return PropertyRead{}, err
		}
		if err := repo.ReplacePeople(ctx, rel, propertyID, users, &actor.UserID); err != nil {
			return PropertyRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: actionFor(rel), Entity: "property", EntityID: &propertyID,
			PropertyID: &propertyID, Before: map[string]any{"user_ids": before}, After: map[string]any{"user_ids": users},
		}); err != nil {
			return PropertyRead{}, err
		}
		return s.read(ctx, repo, actor.TenantID, propertyID)
	})
}

func (s *Service) setProperties(ctx context.Context, actor authz.Actor, rel relation, userID uuid.UUID, props []uuid.UUID) (UserProperties, error) {
	props = uniqueIDs(props)
	return database.InTxValue(ctx, s.db, func(q database.Querier) (UserProperties, error) {
		repo := NewRepository(q)
		n, err := repo.CountMembers(ctx, actor.TenantID, []uuid.UUID{userID})
		if err != nil {
			return UserProperties{}, err
		}
		if n == 0 {
			return UserProperties{}, authz.ErrMemberNotFound
		}
		if err := CheckProperties(ctx, q, props, "body.property_ids"); err != nil {
			return UserProperties{}, err
		}
		before, err := repo.PropertiesOf(ctx, rel, userID)
		if err != nil {
			return UserProperties{}, err
		}
		if err := repo.ReplaceProperties(ctx, rel, userID, props, &actor.UserID); err != nil {
			return UserProperties{}, err
		}
		after, err := repo.PropertiesOf(ctx, rel, userID)
		if err != nil {
			return UserProperties{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: actionFor(rel), Entity: "user", EntityID: &userID,
			Before: map[string]any{"property_ids": before}, After: map[string]any{"property_ids": after},
		}); err != nil {
			return UserProperties{}, err
		}
		return UserProperties{UserID: userID, PropertyIDs: after}, nil
	})
}

func actionFor(rel relation) string {
	if rel == relOwner {
		return "property.owners_changed"
	}
	return "property.assignments_changed"
}

// CheckProperties comprueba que todas las propiedades existan en la empresa
// (q en su schema). La usa tambien la invitacion con property_ids.
func CheckProperties(ctx context.Context, q database.Querier, ids []uuid.UUID, field string) error {
	if len(ids) == 0 {
		return nil
	}
	ids = uniqueIDs(ids)
	n, err := NewRepository(q).CountProperties(ctx, ids)
	if err != nil {
		return err
	}
	if n != len(ids) {
		return ErrUnknownProperties.WithField(field)
	}
	return nil
}

// ApplyInvitation vincula a quien acepta una invitacion con sus propiedades:
// como propietario si su rol es owner y como asignado en cualquier otro. Las
// que se borraron mientras tanto se ignoran. q debe operar en el schema de la
// empresa.
func ApplyInvitation(ctx context.Context, q database.Querier, userID uuid.UUID, owner bool, ids []uuid.UUID, by *uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	rel := relAssigned
	if owner {
		rel = relOwner
	}
	rows, err := q.Query(ctx, `SELECT id FROM properties WHERE id = ANY($1::uuid[]) AND deleted_at IS NULL`, uniqueIDs(ids))
	if err != nil {
		return err
	}
	var live []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		live = append(live, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(live) == 0 {
		return nil
	}
	return NewRepository(q).link(ctx, rel, live, []uuid.UUID{userID}, by)
}
