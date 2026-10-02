from django.views.generic import CreateView, UpdateView, \
        DeleteView, ListView, DetailView
from django.contrib import messages
from django.http import HttpResponseRedirect
from django.core.urlresolvers import reverse
from django.shortcuts import render

def home(request):
    return render(request, 'index.html')
    

def handler_403(request):

    """
    Redirects user home with an error message.
    """

    messages.warning(request, '403 - Forbidden.')
    return HttpResponseRedirect(reverse('home'))


def handler_404(request):

    """
    Redirects user home with an error message.
    """

    messages.warning(request, '404 - Page not found')
    return HttpResponseRedirect(reverse('home'))


def handler_500(request):

    """
    Redirects user home with an error message.
    """

    messages.error(request, '500 - Internal server error.')
    return HttpResponseRedirect(reverse('home'))

class MessageMixin(object):
    """
    Make it easy to display notification messages when using Class Based Views.
    """
    def delete(self, request, *args, **kwargs):
        messages.success(self.request, self.success_message)
        return super(MessageMixin, self).delete(request, *args, **kwargs)

    def form_valid(self, form):
        messages.success(self.request, self.success_message)
        return super(MessageMixin, self).form_valid(form)
        
class RequestCreateView(MessageMixin, CreateView):
    """ 
    Sub-class of the CreateView to automatically pass the Request to the Form. 
    """
    success_message = "Created Successfully"

    def get_form_kwargs(self):
        """ Add the Request object to the Form's Keyword Arguments. """
        kwargs = super(RequestCreateView, self).get_form_kwargs()
        kwargs.update({'request': self.request})
        return kwargs

class RequestUpdateView(MessageMixin, UpdateView):
    """
    Sub-class the UpdateView to pass the request to the form and limit the
    queryset to the requesting user.        
    """
    success_message = "Updated Successfully"

    def get_form_kwargs(self):
        """ Add the Request object to the form's keyword arguments. """
        kwargs = super(RequestUpdateView, self).get_form_kwargs()
        kwargs.update({'request': self.request})
        return kwargs

    def get_queryset(self):
        """ Limit a User to only modifying their own data. """
        qs = super(RequestUpdateView, self).get_queryset()
        return qs.filter(owner=self.request.user)

class RequestDeleteView(MessageMixin, DeleteView):
    """
    Sub-class the DeleteView to restrict a User from deleting other 
    user's data.
    """  
    success_message = "Deleted Successfully"

    def get_queryset(self):
            qs = super(RequestDeleteView, self).get_queryset()
            return qs.filter(owner=self.request.user)