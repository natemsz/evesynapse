import datetime
from django.db import models

#
# Character information
#


class APIKey(models.Model):
    """
    Holds API information for a user.  One to many with user table.
    """

    keyid = models.PositiveIntegerField(help_text="keyID for this character")
    vcode = models.TextField(help_text="vCode for this key")
    expires = models.DateTimeField(help_text="Expiry date for the key")
    accessmask = models.BigIntegerField(help_text="Access mask for this key")
    is_valid = models.BooleanField(help_text="Is this key valid?")
    is_character_key = models.BooleanField(help_text="Is this a character key?  false = corporation key", default=True)
    user = models.ForeignKey('auth.User', help_text="Fkey relationship to user table")

    class Meta(object):
        verbose_name = "API Key"
        verbose_name_plural = "API Keys"


class Character(models.Model):
    """
    Holds information specific to a character.  This is a one-to-many relationship between users & characters
    (ie, a user can have multiple characters, but a character can only have one user).  This stores API key information
    and other useful character-specific info.
    """

    id = models.BigIntegerField(primary_key=True, help_text="Unique key for this character, uses CCP character ID")
    name = models.TextField(help_text="Character name")
    user = models.ForeignKey('auth.User', help_text="FKey relationship to user table")
    apikey = models.ForeignKey('api.APIKey', help_text='FKey relationship to api key table')
    name = models.TextField(help_text="Name of character")
    dob = models.DateTimeField(help_text="DoB of character", default=datetime.datetime.now())
    race = models.TextField(help_text="Race of character", default="")
    bloodline = models.TextField(help_text="Bloodline of character", default="")
    ancestry = models.TextField(help_text="Ancestry of character", default="")
    gender = models.TextField(help_text="Gender", default="Male")
    corp_name = models.TextField(help_text="Name of corporation character is member of", default="")
    corp_id = models.BigIntegerField(help_text="id of corporation", default=0)
    alliance_name = models.TextField(help_text="Name of alliance", default="")
    alliance_id = models.BigIntegerField(help_text="id of alliance", default=0)
    clone_name = models.TextField(help_text="clone level name", default="")
    clone_skill_points = models.PositiveIntegerField(help_text="max SP of clone", default=0)
    balance = models.BigIntegerField(help_text="isk on hand", default=0)
    implant_memory_name = models.TextField(help_text="name of memory implant", default="")
    implant_memory_bonus = models.PositiveIntegerField(help_text="memory bonus", default=0)
    implant_intelligence_name = models.TextField(help_text="name of intelligence implant", default="")
    implant_intelligence_bonus = models.PositiveIntegerField(help_text="intelligence bonus", default=0)
    implant_charisma_name = models.TextField(help_text="name of charisma implant", default="")
    implant_charisma_bonus = models.PositiveIntegerField(help_text="charisma bonus", default=0)
    implant_willpower_name = models.TextField(help_text="name of willpower implant", default="")
    implant_willpower_bonus = models.PositiveIntegerField(help_text="willpower bonus", default=0)
    implant_perception_name = models.TextField(help_text="name of perception implant", default="")
    implant_perception_bonus = models.PositiveIntegerField(help_text="perception bonus", default=0)
    cached_until = models.DateTimeField(help_text="data cached until", default=datetime.datetime.now())

    class Meta(object):
        verbose_name = "Character"
        verbose_name_plural = "Characters"

# API Timer
class APITimer(models.Model):
    """
    Tracking API timers
    """
    character = models.ForeignKey('api.Character', help_text="FKey relationship to character table", null=True, default=None)
    corporation = models.ForeignKey('api.Corp', help_text="FKey relationship to corporation table", null=True, default=None)
    apisheet = models.TextField(help_text="Filename of API Call sheet")
    nextupdate = models.DateTimeField(help_text="Date/Time of next allowed API refresh")

    class Meta(object):
        verbose_name = "API Timer"
        verbose_name_plural = "API Timers"


# Corporation
class Corp(models.Model):
    """
    Table for CorporationSheet information
    """
    
    apikey = models.ForeignKey('api.APIKey', help_text='FKey relationship to api key table')
    corp_id = models.BigIntegerField(help_text="Corporation ID", db_index=True)
    name = models.TextField(help_text="Corporation name")
    ticker = models.TextField(help_text="Corp ticker")
    ceo_id = models.BigIntegerField(help_text="character ID of CEO")
    ceo_name = models.TextField(help_text="CEO Name")
    stastation = models.ForeignKey('eve_db.StaStation', help_text="Station corp headquarters is in")
    description = models.TextField(help_text="Description of corp if provided")
    url = models.TextField(help_text="URL for corporation")
    tax_rate = models.PositiveIntegerField(help_text="Tax rate of corporation")
    member_count = models.PositiveIntegerField(help_text="Number of members of corp")
    member_limit = models.PositiveIntegerField(help_text="Max number of members corp can support")
    shares = models.PositiveIntegerField(help_text="Number of shares of corp outstanding")
    cached_until = models.DateTimeField(help_text="data cached until", default=datetime.datetime.now())

    class Meta(object):
        verbose_name = "Corporation"
        verbose_name_plural = "Corporations"
