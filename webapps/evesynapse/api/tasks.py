# Utility imports
import datetime
import pytz

from celery.task import PeriodicTask

# Models
from api.models import *

# API
from core import eveapi

# API error handling
from api.api_exceptions import handle_api_exception

# Additional exception handling
from django.db import IntegrityError

# eve_db Models
from eve_db.models import StaStation

class ProcessCharacterSheet(PeriodicTask):
    """
    Scan the db an refresh all character sheets
    Currently done once every 5 minutes
    """

    run_every = datetime.timedelta(minutes=5)

    def run(self, **kwargs):
        print "BEGIN CHARACTER IMPORT"

        #define variables
        i_stats = {}
        implant = {}
        attributes = ['memory', 'intelligence', 'perception', 'willpower', 'charisma']

        #grab an api object
        api = eveapi.EVEAPIConnection()

        #scan to see if anyone is due for an update
        update_timers = APITimer.objects.filter(apisheet="CharacterSheet",
                                                nextupdate__lte=pytz.utc.localize(datetime.datetime.utcnow()))
        for update in update_timers:

            character = Character.objects.get(id=update.character_id)
            print ">>> Updating: %s" % character.name

            # Try to fetch a valid key from DB
            try:
                apikey = APIKey.objects.get(id=character.apikey_id, is_valid=True)
            except APIKey.DoesNotExist:
                print('There is no valid key for %s.' % character.name)
                # End execution for this character
                continue

            # Try to authenticate and handle exceptions properly
            try:
                auth = api.auth(keyID=apikey.keyid, vCode=apikey.vcode)
                me = auth.character(character.id)
                sheet = me.CharacterSheet()
                i_stats['name'] = ""
                i_stats['value'] = 0

            except eveapi.Error, e:
                handle_api_exception(e, apikey)

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
                character.alliance_name = sheet.allianceName
                character.alliance_id = sheet.allianceID
            except:
                character.alliance_name = ""
                character.alliance_id = 0

            character.corp_name = sheet.corporationName
            character.corp_id = sheet.corporationID
            character.clone_name = sheet.cloneName
            character.clone_skill_points = sheet.cloneSkillPoints
            character.balance = sheet.balance
            character.implant_memory_name = implant['memory']['name']
            character.implant_memory_bonus = implant['memory']['value']
            character.implant_perception_name = implant['perception']['name']
            character.implant_perception_bonus = implant['perception']['value']
            character.implant_intelligence_name = implant['intelligence']['name']
            character.implant_intelligence_bonus = implant['intelligence']['value']
            character.implant_willpower_name = implant['willpower']['name']
            character.implant_willpower_bonus = implant['willpower']['value']
            character.implant_charisma_name = implant['charisma']['name']
            character.implant_charisma_bonus = implant['charisma']['value']

            character.save()

            # Set nextupdate to cachedUntil
            update.nextupdate = pytz.utc.localize(datetime.datetime.utcfromtimestamp(sheet._meta.cachedUntil))
            update.save()
            print "<<< %s update complete" % character.name
            
            
class ProcessCorporationSheet(PeriodicTask):
    """
    Scan the db an refresh all corporation sheets
    Currently done once every 10 minutes
    Maybe adjust to 5 minutes in the future
    """

    run_every = datetime.timedelta(minutes=10)

    def run(self, **kwargs):
        print "BEGIN CORPORATION IMPORT"
        
        # API Stuff
        api = eveapi.EVEAPIConnection()
        
        # Scan DB for corporation sheets that need an update
        update_timers = APITimer.objects.filter(apisheet="CorporationSheet",
                                            nextupdate__lte=pytz.utc.localize(datetime.datetime.utcnow()))
        for update in update_timers:
        
            corporation = Corp.objects.get(id=update.corporation_id)
            print ">>> Updating: %s" % corporation.name
            
            try:
                apikey = APIKey.objects.get(id=corporation.apikey_id, is_valid=True)
            except APIKey.DoesNotExist:
                print('There is no valid key for %s.' % corporation.name)
                # End execution for this corporation
                continue
                
            # Try to authenticate and handle exceptions properly 
            try:
                auth = api.auth(keyID=apikey.keyid, vCode=apikey.vcode)
                csheet = auth.corp.CorporationSheet()
            except eveapi.Error, e:
                    handle_api_exception(e, apikey)
                    
            corp_id=csheet.corporationID,
            name=csheet.corporationName,
            ticker=csheet.ticker,
            ceo_id=csheet.ceoID,
            ceo_name=csheet.ceoName,
            stastation=StaStation(csheet.stationID),
            description=csheet.description,
            url=csheet.url,
            tax_rate=csheet.taxRate,
            member_count=csheet.memberCount,
            member_limit=csheet.memberLimit,
            shares=csheet.shares
            
            corporation.save()
                    
            # Set nextupdate to cachedUntil
            update.nextupdate = pytz.utc.localize(datetime.datetime.utcfromtimestamp(csheet._meta.cachedUntil))
            update.save()
            print "<<< %s update complete" % corporation.name