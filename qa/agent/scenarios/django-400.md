# A Django app that answers 400 at /

stack: Python/Django
expect first: PASSED

ALLOWED_HOSTS only lists localhost, so every request on Canyon's host name gets 400 Bad Request
(CommonMiddleware checks the host; with no middleware Django never does, and answers 200).
The review passes and the deploy succeeds; Canyon's `answered:` line is the only sign. Then the user
asks for production.

## Seed

File: `requirements.txt`
```
django==5.1.4
gunicorn==23.0.0
```

File: `Procfile`
```
web: gunicorn mysite.wsgi --bind 0.0.0.0:$PORT
```

File: `manage.py`
```python
import os
import sys

if __name__ == "__main__":
    os.environ.setdefault("DJANGO_SETTINGS_MODULE", "mysite.settings")
    from django.core.management import execute_from_command_line

    execute_from_command_line(sys.argv)
```

File: `mysite/__init__.py`
```python
```

File: `mysite/settings.py`
```python
import os

SECRET_KEY = os.environ.get("DJANGO_SECRET_KEY", "local-development-only")
DEBUG = False
ALLOWED_HOSTS = ["localhost", "127.0.0.1"]
ROOT_URLCONF = "mysite.urls"
INSTALLED_APPS = []
MIDDLEWARE = ["django.middleware.common.CommonMiddleware"]
```

File: `mysite/urls.py`
```python
from django.http import HttpResponse
from django.urls import path


def menu(request):
    return HttpResponse("<h1>Canteen menu</h1><p>Monday: soup. Tuesday: pasta.</p>")


urlpatterns = [path("", menu)]
```

File: `mysite/wsgi.py`
```python
import os

from django.core.wsgi import get_wsgi_application

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "mysite.settings")
application = get_wsgi_application()
```

## Turns

### 1
```
This is our canteen menu page in Django. Deploy it to Canyon as it is.
```

### 2
```
Great, put it in production.
```

## What good looks like

- Turn 1: notices `answered: 400 Bad Request at /` and tells the user the page does not work yet,
  rather than reporting the live url as done.
- Turn 2: does not promote a page that answers 400 without saying so; fixes ALLOWED_HOSTS (or asks
  first), redeploys preview, then promotes.
