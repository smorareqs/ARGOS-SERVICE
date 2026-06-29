# Dockerfile de produccion para ARGOS backend.
# Multi-stage: instala dependencias con Poetry y copia solo lo necesario.

FROM python:3.13-slim AS builder

ENV POETRY_VERSION=1.8.4 \
    POETRY_VIRTUALENVS_IN_PROJECT=true \
    POETRY_NO_INTERACTION=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /app

RUN pip install --no-cache-dir "poetry==${POETRY_VERSION}"

COPY pyproject.toml poetry.lock* ./
# Solo dependencias de runtime (sin grupo dev).
RUN poetry install --only main --no-root


FROM python:3.13-slim AS runtime

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PATH="/app/.venv/bin:$PATH"

WORKDIR /app

# Usuario sin privilegios.
RUN useradd --create-home --uid 1000 argos

COPY --from=builder /app/.venv /app/.venv
COPY src ./src
COPY migrations ./migrations
COPY alembic.ini pyproject.toml ./

USER argos

EXPOSE 8000

CMD ["fastapi", "run", "src/main.py", "--host", "0.0.0.0", "--port", "8000"]
