from django.conf.urls import patterns, include, url
from django.contrib import admin
from django.views.generic.simple import direct_to_template
admin.autodiscover()

handler403 = 'core.views.handler_403'
handler404 = 'core.views.handler_404'
handler500 = 'core.views.handler_500'

urlpatterns = patterns('',
    url(r'^$', 'core.views.home', name='home'),
    url(r'^test/$', direct_to_template, {'template': 'index2.html'}),
    url(r'^admin/', include(admin.site.urls)),
    url(r'^accounts/', include('auth.urls')),
    url(r'^accounts/', include('api.urls')),
    url(r'^robots\.txt$', direct_to_template,
        {'template': 'robots.txt', 'mimetype': 'text/plain'}),
)