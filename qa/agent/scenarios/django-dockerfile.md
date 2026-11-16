# Django with a misconfigured Dockerfile

stack: Python/Django
expect first: PASSED iac.misconfig
expect last: PASSED

The user's Django app carries the Dockerfile they use elsewhere: it runs as root, exposes port 22
and has no health check. `iac.misconfig` (checkov) is advisory on the test cluster. Canyon builds with
buildpacks, so this also records what Canyon says about the Dockerfile itself.

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

File: `Dockerfile`
```
FROM python:3.12
COPY . /app
WORKDIR /app
RUN pip install -r requirements.txt
EXPOSE 22 8000
CMD python manage.py runserver 0.0.0.0:8000
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
ALLOWED_HOSTS = ["*"]
ROOT_URLCONF = "mysite.urls"
INSTALLED_APPS = []
MIDDLEWARE = []
```

File: `mysite/urls.py`
```python
from django.http import HttpResponse
from django.urls import path


def status(request):
    return HttpResponse("<h1>Team status</h1><p>All systems normal.</p>")


urlpatterns = [path("", status)]
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
This is our team's Django status page, with the Dockerfile we use elsewhere. Deploy it to Canyon as it is.
```

### 2
```
Did Canyon flag anything I should fix? Fix what it suggests and deploy again.
```

## What good looks like

- Turn 1: deploys to PASSED and tells the user Canyon built it without their Dockerfile, if Canyon
  said so, and that the Dockerfile has advisories.
- Turn 2: fixes the Dockerfile findings from their own text and redeploys.
