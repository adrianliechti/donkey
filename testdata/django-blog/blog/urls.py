from django.contrib import admin
from django.http import JsonResponse
from django.urls import path


def index(request):
    return JsonResponse({"app": "django-blog", "hint": "the admin is at /admin/"})


urlpatterns = [
    path("", index),
    path("admin/", admin.site.urls),
]
