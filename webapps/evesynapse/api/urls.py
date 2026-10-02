from django.conf.urls.defaults import patterns, url

urlpatterns = patterns('api.views',
    #
    # Account management
    #

    # Settings
    url(r'^api/pilots/$', 'characters', name='manage_characters'),
    url(r'^api/pilots/remove/(?P<char_id>[0-9]+)/$', 'remove_character', name='remove_character'),
    url(r'^api/key/$', 'api_key', name='manage_api_keys'),
    url(r'^api/key/remove/(?P<apikey_id>[0-9]+)/$', 'remove_api_key', name='remove_api_key'),
    url(r'^api/key/refresh/(?P<apikey_id>[0-9]+)/$', 'refresh_api_key', name='refresh_api_key'),
    url(r'^api/pilot/(?P<api_id>[0-9]+)/(?P<api_verification_code>[a-zA-Z0-9]+)/$', 'api_character', name='add_characters'),
    url(r'^api/pilot/(?P<char_id>[0-9]+)/$', 'pilot_sheet', name='pilot_sheet')
)
