"""Django settings — the generator's output with the three deploy-critical
lines taken off their defaults.

Each of these is a setting nobody thinks about until it breaks something, and
each breaks silently:

  ALLOWED_HOSTS  Django answers *400 Bad Request* to every request carrying a
                 Host header it doesn't know — which is every request through
                 a gateway. Empty is the default, and the app looks perfectly
                 healthy while refusing everyone.
  DEBUG          the debug page prints settings, including secrets, to anyone
                 who triggers an error.
  SECRET_KEY     a key committed to the repository is not a secret.

The sqlite file below is deliberately left where the generator puts it, in
the project folder: it is real data in a place that does not survive a
restart, which is for donkey to notice and put on a disk.
"""

import os
from pathlib import Path

BASE_DIR = Path(__file__).resolve().parent.parent

SECRET_KEY = os.environ.get("SECRET_KEY", "django-insecure-local-development-only")
DEBUG = os.environ.get("DEBUG") == "1"

# A comma-separated list, or "*" to accept whatever address the platform gives
# this app. "*" is safe here because nothing routes to it except the gateway.
ALLOWED_HOSTS = os.environ.get("ALLOWED_HOSTS", "*").split(",")

# Django checks the Origin header on unsafe requests, and behind TLS it must
# be told which origins are its own.
CSRF_TRUSTED_ORIGINS = [
    o for o in os.environ.get("CSRF_TRUSTED_ORIGINS", "").split(",") if o
]

INSTALLED_APPS = [
    "django.contrib.admin",
    "django.contrib.auth",
    "django.contrib.contenttypes",
    "django.contrib.sessions",
    "django.contrib.messages",
    "django.contrib.staticfiles",
]

MIDDLEWARE = [
    "django.middleware.security.SecurityMiddleware",
    "django.contrib.sessions.middleware.SessionMiddleware",
    "django.middleware.common.CommonMiddleware",
    "django.middleware.csrf.CsrfViewMiddleware",
    "django.contrib.auth.middleware.AuthenticationMiddleware",
    "django.contrib.messages.middleware.MessageMiddleware",
]

ROOT_URLCONF = "blog.urls"
WSGI_APPLICATION = "blog.wsgi.application"

TEMPLATES = [
    {
        "BACKEND": "django.template.backends.django.DjangoTemplates",
        "DIRS": [],
        "APP_DIRS": True,
        "OPTIONS": {
            "context_processors": [
                "django.template.context_processors.request",
                "django.contrib.auth.context_processors.auth",
                "django.contrib.messages.context_processors.messages",
            ]
        },
    }
]

DATABASES = {
    "default": {
        "ENGINE": "django.db.backends.sqlite3",
        "NAME": BASE_DIR / "db.sqlite3",
    }
}

STATIC_URL = "static/"
STATIC_ROOT = BASE_DIR / "staticfiles"
DEFAULT_AUTO_FIELD = "django.db.models.BigAutoField"
