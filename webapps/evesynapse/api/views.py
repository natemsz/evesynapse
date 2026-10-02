# Utility imports
import datetime
import pytz

# Template and context-related imports
from django.core.urlresolvers import reverse
from django.shortcuts import render_to_response
from django.http import HttpResponseRedirect
from django.template import RequestContext
from django.contrib import messages
from django.contrib.auth.decorators import login_required
from django.conf import settings
from django.views.decorators.cache import cache_control
# API Models
from api.models import APIKey, Character, APITimer, Corp

# Forms
from api.forms import APIKeyForm

# API
from core import eveapi

# Char management utilities
from api.util import manage_character_api_timers
from api.util import manage_corporation_api_timers

# eve_db Models
from eve_db.models import StaStation


@login_required
@cache_control(must_revalidate=True, private=True)
def characters(request):
    characters = Character.objects.filter(user=request.user)

    if not characters:
        # Message and redirect
        messages.info(request, 'Currently there are no characters associated with your account. You need to add them via an API key first.')
        return HttpResponseRedirect(reverse('manage_api_keys'))

    rcontext = RequestContext(request, {'characters': characters})
    return render_to_response('api/characters.html', rcontext)


@login_required
@cache_control(must_revalidate=True, private=True)
def remove_character(request, char_id):
    try:
        # Delete only matching character to prevent unauthorized deletions
        char = Character.objects.get(user=request.user, id=char_id)
        char.delete()
    except:
        # Message and redirect
        messages.error(request, 'There is no such character.')
        return HttpResponseRedirect(reverse('manage_characters'))

    # Message and redirect
    messages.info(request, 'Character was removed.')
    return HttpResponseRedirect(reverse('manage_characters'))


@login_required
@cache_control(must_revalidate=True, private=True)
def api_key(request):
    # Get Keys
    char_keys = APIKey.objects.filter(user=request.user, is_character_key=True)
    corp_keys = APIKey.objects.filter(user=request.user, is_character_key=False)

    # Form
    if request.method == 'POST':
        form = APIKeyForm(request.POST)
        if form.is_valid():

            # Add success message
            messages.success(request, 'Your key is valid.')
            # Redirect home
            return HttpResponseRedirect(reverse('add_characters', kwargs={'api_id': form.cleaned_data.get('api_id'),
                                                                          'api_verification_code': form.cleaned_data.get('api_verification_code')}))
    else:
        form = APIKeyForm()

    rcontext = RequestContext(request, {})
    return render_to_response('api/api_key.html', {'form': form, 'char_keys': char_keys, 'corp_keys': corp_keys}, rcontext)


@login_required
@cache_control(must_revalidate=True, private=True)
def remove_api_key(request, apikey_id):
    try:
        # Delete only matching character to prevent unauthorized deletions
        key = APIKey.objects.get(user=request.user, keyid=apikey_id)
        is_character_key = key.is_character_key
        key.delete()
    except:
        # Message and redirect
        messages.error(request, 'There is no such key.')
        return HttpResponseRedirect(reverse('manage_api_keys'))

    # Message and redirect
    if is_character_key:
        messages.info(request, 'The key and all associated characters were removed.')
    else:
        messages.info(request, 'The key was removed.')

    return HttpResponseRedirect(reverse('manage_api_keys'))


@login_required
@cache_control(must_revalidate=True, private=True)
def refresh_api_key(request, apikey_id):
    """
    Refreshes the selected key. Updates API timers for that key too.
    """

    try:
        # Refresh that key
        key = APIKey.objects.get(user=request.user, keyid=apikey_id)

        # Try to authenticate with supplied key / ID pair and fetch api key meta data.
        try:
            # Fetch info
            api = eveapi.EVEAPIConnection()
            auth = api.auth(keyID=key.keyid, vCode=key.vcode)
            key_info = auth.account.APIKeyInfo()
        except:
            # Message and redirect
            messages.error(request, """Verification of your API key failed.
                                       Please try again.""")
            return HttpResponseRedirect(reverse('manage_api_keys'))

        # Do a simple bitwise operation to determine if we have sufficient rights with this key.
        min_access_mask = 8
        if not ((min_access_mask & key_info.key.accessMask) == min_access_mask):
            # Message and redirect
            messages.error(request, """The API key you supplied does not have sufficient rights.
                                       Please try again """)
            return HttpResponseRedirect(reverse('manage_api_keys'))

        # Handle keys which never expire
        try:
            key_expiration = datetime.datetime.fromtimestamp(key_info.key.expires)
        except:
            key_expiration = "9999-12-31 00:00:00"

        key.expires = key_expiration
        key.accessmask = key_info.key.accessMask
        key.is_valid = True

        if key_info.key.type == "Character" or key_info.key.type == "Account":
            key.is_character_key = True
        else:
            key.is_character_key = False

        # Save
        key.save()

        # Refresh characters
        chars = Character.objects.filter(apikey=key)

        for char in chars:
            manage_character_api_timers(char)

        # Message and redirect
        messages.success(request, 'Successfully refreshed API key and all associated characters.')

    except:
        # Message and redirect
        messages.error(request, 'There is no such key.')
        return HttpResponseRedirect(reverse('manage_api_keys'))

    return HttpResponseRedirect(reverse('manage_api_keys'))


@login_required
@cache_control(must_revalidate=True, private=True)
def api_character(request, api_id, api_verification_code):

    """
    Validate key / ID combination. If it's valid, check security bitmask.
    """

    # Try to authenticate with supplied key / ID pair and fetch api key meta data.
    try:
        # Fetch info
        api = eveapi.EVEAPIConnection()
        auth = api.auth(keyID=api_id, vCode=api_verification_code)
        key_info = auth.account.APIKeyInfo()
    except:
        # Message and redirect
        messages.error(request, """Verification of your API key failed.
                                   Please try again.""")
        return HttpResponseRedirect(reverse('manage_api_keys'))

    if key_info.key.type == "Character" or key_info.key.type == "Account":

        # Minimum access mask
        min_access_mask = 8

        # attributes & implants
        implant = {}
        i_stats = {}
        attributes = ['memory', 'intelligence', 'perception', 'willpower', 'charisma']

        # Do a simple bitwise operation to determine if we have sufficient rights with this key.
        if not ((min_access_mask & key_info.key.accessMask) == min_access_mask):
            # Message and redirect
            messages.error(request, """The API key you supplied does not have sufficient rights.
                                       Please try again.""")
            return HttpResponseRedirect(reverse('manage_api_keys'))

        # Get characters associated with this key
        characters = auth.account.Characters().characters

        # If form is submitted, add characters to account
        if request.method == 'POST':
            post_characters = request.POST.getlist('characters')

            added_chars = False

            for char in characters:
                if str(char.characterID) in post_characters:
                    # Add key to DB if it does not exist
                    if not APIKey.objects.filter(keyid=api_id, vcode=api_verification_code):

                        # Handle keys which never expire
                        try:
                            key_expiration = datetime.datetime.fromtimestamp(key_info.key.expires)
                        except:
                            key_expiration = "9999-12-31 00:00:00"

                        key = APIKey(user=request.user,
                                     keyid=api_id,
                                     vcode=api_verification_code,
                                     expires=key_expiration,
                                     accessmask=key_info.key.accessMask,
                                     is_valid=True,
                                     is_character_key=True)

                        key.save()

                    else:
                        try:
                            key = APIKey.objects.get(user=request.user, keyid=api_id, vcode=api_verification_code)
                        except:
                            messages.error(request, """The key you entered is already in use by another EveSynapse account. 
                                                       Please enter a unique key and try again.""")
                            return HttpResponseRedirect(reverse('manage_api_keys'))

                    # If char already is assigned to another key, just move it
                    try:
                        char_to_move = Character.objects.get(id=char.characterID, user=request.user)
                        char_to_move.apikey_id = key.id
                        char_to_move.save()

                        # Update timers
                        manage_character_api_timers(char_to_move)

                        added_chars = True

                    except Character.DoesNotExist:
                        # Add character
                        me = auth.character(char.characterID)
                        sheet = me.CharacterSheet()
                        i_stats['name'] = ""
                        i_stats['value'] = 0
                        for attr in attributes:
                            implant[attr] = i_stats

                        # have to check because if you don't have an implant in you get nothing back
                        try:
                            implant['memory'] = {'name': sheet.attributeEnhancers.memoryBonus.augmentatorName,
                                                 'value': sheet.attributeEnhancers.memoryBonus.augmentatorValue}
                        except:
                            pass
                        try:
                            implant['perception'] = {'name': sheet.attributeEnhancers.perceptionBonus.augmentatorName,
                                                     'value': sheet.attributeEnhancers.perceptionBonus.augmentatorValue}
                        except:
                            pass
                        try:
                            implant['intelligence'] = {'name': sheet.attributeEnhancers.intelligenceBonus.augmentatorName,
                                                       'value': sheet.attributeEnhancers.intelligenceBonus.augmentatorValue}
                        except:
                            pass
                        try:
                            implant['willpower'] = {'name': sheet.attributeEnhancers.willpowerBonus.augmentatorName,
                                                    'value': sheet.attributeEnhancers.willpowerBonus.augmentatorValue}
                        except:
                            pass
                        try:
                            implant['charisma'] = {'name': sheet.attributeEnhancers.charismaBonus.augmentatorName,
                                                   'value': sheet.attributeEnhancers.charismaBonus.augmentatorValue}
                        except:
                            pass
                        try:
                            a_name = sheet.allianceName
                            a_id = sheet.allianceID

                        except:
                            a_name = ""
                            a_id = 0

                        new_char = Character(id=char.characterID,
                                            name=char.name,
                                            user=request.user,
                                            apikey=key,
                                            corp_name=sheet.corporationName,
                                            corp_id=sheet.corporationID,
                                            alliance_name=a_name,
                                            alliance_id=a_id,
                                            dob=pytz.utc.localize(datetime.datetime.utcfromtimestamp(sheet.DoB)),
                                            race=sheet.race,
                                            bloodline=sheet.bloodLine,
                                            ancestry=sheet.ancestry,
                                            gender=sheet.gender,
                                            clone_name=sheet.cloneName,
                                            clone_skill_points=sheet.cloneSkillPoints,
                                            balance=sheet.balance,
                                            implant_memory_name=implant['memory']['name'],
                                            implant_memory_bonus=implant['memory']['value'],
                                            implant_perception_name=implant['perception']['name'],
                                            implant_perception_bonus=implant['perception']['value'],
                                            implant_intelligence_name=implant['intelligence']['name'],
                                            implant_intelligence_bonus=implant['intelligence']['value'],
                                            implant_willpower_name=implant['willpower']['name'],
                                            implant_willpower_bonus=implant['willpower']['value'],
                                            implant_charisma_name=implant['charisma']['name'],
                                            implant_charisma_bonus=implant['charisma']['value'])
                        new_char.save()

                        new_apitimer = APITimer(character=new_char,
                                                corporation=None,
                                                apisheet="CharacterSheet",
                                                nextupdate=pytz.utc.localize(datetime.datetime.utcfromtimestamp(sheet._meta.cachedUntil)))
                        new_apitimer.save()

                        # Update other timers
                        manage_character_api_timers(new_char)
                        
                        added_chars = True

            # Change message depending on what we did
            if added_chars:
                messages.success(request, "Successfully added the selected character(s) to your account.")
            else:
                messages.info(request, "No characters were added.")
            return HttpResponseRedirect(reverse('manage_characters'))

    else:
        # This must be a corporation key then
        # Add key to DB if it does not exist
        if not APIKey.objects.filter(keyid=api_id, vcode=api_verification_code):

            # Handle keys which never expire
            try:
                key_expiration = datetime.datetime.fromtimestamp(key_info.key.expires)
            except:
                key_expiration = "9999-12-31 00:00:00"

            key = APIKey(user=request.user,
                         keyid=api_id,
                         vcode=api_verification_code,
                         expires=key_expiration,
                         accessmask=key_info.key.accessMask,
                         is_valid=True,
                         is_character_key=False)

            key.save()
            
            mcorp = auth.corp.CorporationSheet()
            
            # Check if corporation entry exists in DB and if not, add it
            try:
                corporation = Corp.objects.get(corp_id=mcorp.corporationID)
            except Corp.DoesNotExist:    
                # Add corporation                
                new_corp = Corp(apikey=key,
                                corp_id=mcorp.corporationID,
                                name=mcorp.corporationName,
                                ticker=mcorp.ticker,
                                ceo_id=mcorp.ceoID,
                                ceo_name=mcorp.ceoName,
                                stastation=StaStation(mcorp.stationID),
                                description=mcorp.description,
                                url=mcorp.url,
                                tax_rate=mcorp.taxRate,
                                member_count=mcorp.memberCount,
                                member_limit=mcorp.memberLimit,
                                shares=mcorp.shares)
                new_corp.save()
                
                new_apitimer = APITimer(character=None,
                                        corporation=new_corp,
                                        apisheet="CorporationSheet",
                                        nextupdate=pytz.utc.localize(datetime.datetime.utcfromtimestamp(mcorp._meta.cachedUntil)))
                
                manage_corporation_api_timers(new_corp)

            messages.success(request, "Successfully added your corporate key.")

        else:
            messages.info(request, "No keys were added.")

        return HttpResponseRedirect(reverse('manage_api_keys'))

    rcontext = RequestContext(request, {'chars': characters})
    return render_to_response('api/api_character.html', rcontext)
    
@login_required
@cache_control(must_revalidate=True, private=True)
def pilot_sheet(request, char_id):

    try:
        char = Character.objects.get(user=request.user, id=char_id)
    except:
        messages.error(request, 'There is no such pilot in our database.')
        return HttpResponseRedirect(reverse('home'))

    rcontext = RequestContext(request, {'char': char, 'next_update': APITimer.objects.get(character_id=char.id,
                                                                              apisheet='CharacterSheet').nextupdate})
    return render_to_response('api/pilot_sheet.html', rcontext)
