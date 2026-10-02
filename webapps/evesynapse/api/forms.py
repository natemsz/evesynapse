"""
Forms and validation code for user settings.
"""

from django.contrib.auth.models import User
from django import forms

# Authentication
from django.contrib.auth import authenticate

# API
from core import eveapi

# I put this on all required fields, because it's easier to pick up
# on them with CSS or JavaScript if they have a class of "required"
# in the HTML. Your mileage may vary. If/when Django ticket #3515
# lands in trunk, this will no longer be necessary.
attrs_dict = {'class': 'input-xlarge required'}

class APIKeyForm(forms.Form):
    """
    API key submission form.
    """

    # API Key
    api_id = forms.IntegerField(widget=forms.TextInput(attrs={'class': 'span2 input-xlarge required'}))
    api_verification_code = forms.CharField(widget=forms.TextInput(attrs=attrs_dict))

    def clean(self):

        """
        Validate key / ID combination. If it's valid, check security bitmask.
        """
        api_id = self.cleaned_data.get("api_id")
        api_verification_code = self.cleaned_data.get("api_verification_code")

        # Try to authenticate with supplied key / ID pair and fetch api key meta data.
        try:
            # Fetch info
            api = eveapi.EVEAPIConnection()
            auth = api.auth(keyID=api_id, vCode=api_verification_code)
            key_info = auth.account.APIKeyInfo()
        except:
            raise forms.ValidationError("Verification of your API key failed. Please follow the instructions on the right half of this page to generate a valid one.")

        return self.cleaned_data
