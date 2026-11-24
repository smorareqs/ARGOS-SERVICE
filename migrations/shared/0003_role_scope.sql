-- Alcance por asignacion de cada rol (APP/docs/plans/fase02.md §3, PRD §2.2 capa 3).
-- Un rol con scoped = true solo ve las propiedades que tiene asignadas
-- (property_assignments) o de las que es propietario (property_owners); el resto
-- ve todas las de la empresa. Los roles de sistema toman el valor de la matriz
-- en codigo (authz.SystemRoles); los personalizados lo elige la empresa.

-- migrate:up
ALTER TABLE shared.roles ADD COLUMN scoped boolean NOT NULL DEFAULT false;

UPDATE shared.roles SET scoped = true
WHERE is_system AND key IN ('cleaner', 'maintenance', 'owner', 'guest');

-- migrate:down
ALTER TABLE shared.roles DROP COLUMN scoped;
