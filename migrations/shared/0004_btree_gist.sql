-- Extension btree_gist (APP/docs/plans/fase04.md §2): permite restricciones
-- EXCLUDE que mezclan igualdad de uuid con solapamiento de rangos, para que la
-- base rechace dos reservas de la misma propiedad que se cruzan (0006, Persona
-- A) o dos asignaciones del mismo parqueo (0007, Persona B).
--
-- Va en shared y no en cada tenant: una extension existe una vez por base de
-- datos, y si se creara desde un tenant quedaria dentro de su schema (al
-- borrar esa empresa se borraria para todas). Las clases de operadores por
-- defecto se encuentran sin importar el search_path.

-- migrate:up
CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA shared;

-- migrate:down
DROP EXTENSION IF EXISTS btree_gist;
